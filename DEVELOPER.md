# Locutus – developer guide

This guide is for developers who want to understand and change the code of Locutus. For what
Locutus does and how to install and use it, see [README.md](README.md) and
[QuickStart.md](QuickStart.md); this guide assumes you have read the first section of the README
(*How Locutus works*: systems, repositories, clients, roles).

## Contents

1. [The big picture](#the-big-picture)
2. [Repository layout](#repository-layout)
3. [Dependencies](#dependencies)
4. [Startup](#startup)
5. [An HTTP request, from start to end](#an-http-request-from-start-to-end)
6. [Jobs: backup, prune, delete and restore](#jobs-backup-prune-delete-and-restore)
7. [The borg layer](#the-borg-layer)
8. [The Locutus client container](#the-locutus-client-container)
9. [Data model and migrations](#data-model-and-migrations)
10. [Security model](#security-model)
11. [Views, front end and translations](#views-front-end-and-translations)
12. [Design patterns](#design-patterns)
13. [Code style and conventions](#code-style-and-conventions)
14. [Building, running and testing](#building-running-and-testing)
15. [Recipes for common changes](#recipes-for-common-changes)

## The big picture

Locutus is a single Go program with a server-rendered web interface. It does not implement backups
itself: it drives the `borg` command line client, locally in its container and over SSH on clients.

```
 browser ──HTTP──▶ controllers ──▶ models ──▶ SQLite (locutus.db)
                       │    │
                       │    └──▶ views (html/template + i18n)
                       ▼
                   scheduler ──▶ borg.Service ──▶ borg (local process)
                  (background         │              │ SSH via BORG_RSH + in-memory ssh-agent
                    jobs)             │              ▼
                                      │          repository server (borg serve)
                                      └──▶ remote ──SSH──▶ client (borg create / tar / stat)
```

The repository builds **two binaries**, released together with the same version:

| Binary | Source | Image (Dockerfile target) | Role |
|--------|--------|---------------------------|------|
| `locutus` | [main.go](main.go) | `locutus` (default) | The web application. |
| `locutus-client` | [cmd/locutus-client/main.go](cmd/locutus-client/main.go) | `client` | Entrypoint of the Locutus client container: sets up sshd, the user `borg`, sudo and the wrapper, then `exec`s sshd. Locutus pushes its configuration with `locutus-client apply`. |

There are no other services: no message queue, no separate worker, no JavaScript build. All
templates, static files and translations are embedded in the binary with `go:embed`.

## Repository layout

```
main.go                        wiring of the web application (see Startup)
cmd/locutus-client/main.go     entrypoint of the client container
internal/
  config/      environment variables (LOCUTUS_*) → *config.Config
  db/          opens SQLite, applies the migrations (the whole schema is here)
  models/      domain types + all SQL ("model" of MVC); Store is the single data access object
  controllers/ HTTP handlers, routes and middleware ("controller" of MVC)
  views/       html/template rendering, embedded templates/ and static/ ("view" of MVC)
  i18n/        translator + locales/en.json and locales/nl.json
  scheduler/   background loop for scheduled jobs, runs all jobs, mails their output
  schedule/    cron expressions (parse, next run) and the presets of the schedule editor
  borg/        everything borg: command lines (Driver), running borg, archive tree, restore pipeline
  remote/      SSH client: run a command on a host, known_hosts (trust on first use)
  client/      configuration and setup logic of the client container (used by cmd/locutus-client)
  auth/        password hashing, OIDC and SAML login, random tokens
  secrets/     AES-256-GCM encryption of secrets stored in the database
  mail/        SMTP (STARTTLS / SMTPS) for mailing job output
  tlsutil/     extra CA certificates for outgoing TLS
  version/     Version (edited by hand) and Build (set with -ldflags)
contrib/locutus-borg-wrapper.sh   optional forced command for the Locutus key on clients
docker/mkrootfs.sh                builds the Alpine root file system of both images
Dockerfile, build.sh, docker-compose.yml, client/docker-compose.yml
```

Dependencies between the internal packages only go "downwards":

```
main ─▶ controllers ─▶ scheduler ─▶ borg ─▶ remote
            │              │         
            ├─▶ views ─────┼─▶ models ─▶ secrets, mail
            ├─▶ auth       ├─▶ schedule
            ├─▶ client     └─▶ i18n
            └─▶ config ─▶ tlsutil
```

`models` knows nothing about HTTP or borg; `borg` knows nothing about the database (it receives
plain structs such as `borg.Repo` and `borg.Client`). The glue between the two is in `scheduler`
(`scheduler.RepoFor`) and in the controllers.

## Dependencies

### Go modules ([go.mod](go.mod))

The dependency list is deliberately short. Prefer the standard library; add a module only when
the alternative is implementing a protocol yourself.

| Module | Used for | Where |
|--------|----------|-------|
| `modernc.org/sqlite` | SQLite driver in pure Go, so the binary builds with `CGO_ENABLED=0` and cross-compiles without a C toolchain | `db` |
| `golang.org/x/crypto` | `ssh` (SSH client, key parsing), `ssh/agent` (in-memory agent), `bcrypt` (passwords) | `remote`, `borg`, `auth`, `controllers`, `client` |
| `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` | OpenID Connect login | `auth/oidc.go` |
| `github.com/crewjam/saml` | SAMLv2 login | `auth/saml.go` |

Everything else (HTTP routing, templates, JSON, TLS, SMTP, tar, cron) is the standard library. The
router is `net/http.ServeMux` with the method and wildcard patterns of Go 1.22+ (`"GET /systems/{id}"`,
`r.PathValue("id")`).

### Runtime (in the images)

Both images are `FROM scratch` with an Alpine root file system that `docker/mkrootfs.sh` builds
for the target architecture.

- **Locutus image**: `borgbackup`, `openssh-client` (borg uses `ssh` via `BORG_RSH`), `tzdata`, CA
  certificates.
- **Client image**: additionally `openssh-server`, `sudo`, GNU `tar`, `coreutils` (`stat`) and
  `findutils` (`xargs`), used by restores.

Some packages come from Alpine edge (`@edge`) because stable has no fix for known CVEs yet; the
Dockerfile says which and when to drop the tags.

### On the machines Locutus manages

Remote clients need `borg`, `tar` and `stat`; repository servers need `borg`. Locutus supports
**borg 1.x** only (`borg compact` is used when the local borg is 1.2 or newer).

## Startup

[main.go](main.go) is pure wiring, in this order:

1. `config.Load()` reads all `LOCUTUS_*` variables (plus `TZ`) with their defaults.
2. Optional extra CA certificates are installed as the default trust pool (`tlsutil`).
3. `secrets.Open` loads the encryption key (`LOCUTUS_SECRET_KEY`, or creates `secret.key`).
4. `db.Open` opens SQLite and runs the migrations; `models.NewStore(db, box)` wraps it.
5. `i18n.New` and `views.New` load the embedded translations and templates. A template that does
   not parse stops the program here, not on the first request.
6. `borg.NewService` prepares the borg base directory and the known_hosts file.
7. `scheduler.New(...)` and `go sched.Run(ctx)` start the background loop.
8. `controllers.New(...)` creates the `App` (which also starts `purgeLoop`), and `app.Routes()`
   becomes the handler of the `http.Server` (HTTP or HTTPS).
9. On SIGINT/SIGTERM: stop the scheduler loop, shut down HTTP (10 s), wait for running jobs (30 s).

All dependencies are created once and passed down explicitly: there are no package-level
singletons apart from immutable tables (and `pushLocks` in `controllers/containers.go`).

## An HTTP request, from start to end

### Middleware

[controllers/app.go](internal/controllers/app.go) builds the chain, outermost first:

```
securityHeaders → loadSession → requireSetup → checkCSRF → ServeMux → [requireRole →] handler
```

| Middleware | Does |
|------------|------|
| `securityHeaders` | CSP (`default-src 'self'`, so no inline scripts or styles), `X-Frame-Options`, HSTS when HTTPS |
| `loadSession` | session cookie → `models.User` in the request context; read it with `currentUser(r)` |
| `requireSetup` | redirects everything to `/setup` until the first user exists (cached in `setupDone`) |
| `checkCSRF` | every POST must carry the `csrf` form field equal to the `locutus_csrf` cookie (double submit cookie); only the SAML ACS endpoint is exempt |
| `requireLogin` / `requireRole` | per route; wrapped via the small helpers `admin(...)`, `view(...)`, `restore(...)`, `backup(...)` in `Routes()` |

### Route groups

All routes are listed in one place, `App.Routes()`. Reading that function is the fastest way to
find the handler of a page.

- `/setup`, `/login`, `/logout`, `/language`, `/account`, `/auth/...` — authentication ([auth.go](internal/controllers/auth.go)).
- `/admin/...` — admin only: users and identity providers ([admin.go](internal/controllers/admin.go)),
  systems ([admin_systems.go](internal/controllers/admin_systems.go)), client containers
  ([containers.go](internal/controllers/containers.go)), logging ([admin_logging.go](internal/controllers/admin_logging.go)),
  mail ([admin_mail.go](internal/controllers/admin_mail.go)). **The admin has no access to backups or files.**
- `/systems/...` — viewers, restorers and backuppers, only for *assigned* systems: overview, archive
  browsing, download, restore, job detail ([systems.go](internal/controllers/systems.go)), and
  backup/prune schedules and "run now" ([plans.go](internal/controllers/plans.go)).

Role checks (`requireRole`) answer *may this role do this kind of thing*. Whether the user may
touch *this* system is checked in the handler with `userSystem` (or `planClient` / `loadPlan`,
which build on it): it returns 404 for systems that are not assigned to the user. Every new handler
under `/systems/{id}` must start with one of these.

### Shape of a handler

Handlers follow the same pattern. A typical POST handler (simplified from `planRunNow`):

```go
func (a *App) planRunNow(w http.ResponseWriter, r *http.Request) {
	s, p := a.loadPlan(w, r)            // load + authorise; on failure it has already rendered an error
	if s == nil {
		return
	}
	if len(p.Paths) == 0 {              // validate; user errors become a flash message + redirect
		a.flash(w, r, "error", "error.paths_required")
		http.Redirect(w, r, "/systems/"+itoa(s.ID)+"/schedule/backup", http.StatusSeeOther)
		return
	}
	job, err := a.sched.Start(scheduler.StartRequest{...})
	if errors.Is(err, scheduler.ErrBusy) { ... }
	if err != nil {
		a.serverError(w, r, err)        // logs the error, shows a generic page
		return
	}
	http.Redirect(w, r, "/systems/"+itoa(s.ID)+"/jobs/"+itoa(job.ID), http.StatusSeeOther)
}
```

Conventions that follow from this:

- **Post/Redirect/Get**: a successful POST always redirects (303 See Other). One-time messages
  travel in the `locutus_flash` cookie (`a.flash` / `popFlash`), already translated.
- **Form errors** re-render the form with status 422 and `Page.Errors` (a list of translated
  messages), keeping what the user typed.
- **Helpers that can fail write the response themselves** and return `nil`/`false`
  (`userSystem`, `systemFromPath`, `loadPlan`, `archiveFromPath`, …). The caller just returns.
- Error helpers: `a.errorPage(w, r, status, key)`, `a.storeError` (maps `models.ErrNotFound` to
  404), `a.serverError` (logs, 500). Never show internal error text to the user, except borg or
  SSH output on pages that are meant to show it (test results, job output).
- Render with `a.render(w, r, status, "template-name", &views.Page{Title: ..., Data: ...})`. `render`
  fills the common fields (user, language, CSRF token, flash, path). Page-specific data goes in
  `Data`, usually a small struct defined next to the handler.
- Form input: `formText` (trimmed), `formID`, `formIDs`, `formInt`, `pathID`.

## Jobs: backup, prune, delete and restore

Everything that runs borg for longer than a moment is a **job**: a row in the `jobs` table with
its kind, status and the complete output. Jobs run in the background in
[scheduler/scheduler.go](internal/scheduler/scheduler.go); the browser is redirected to the job
page, which refreshes while the job runs.

### Starting a job

```
scheduled:  Scheduler.Run ─(every 20 s)─▶ checkPlans ─▶ Start(StartRequest{Trigger: schedule})
manual:     controller ─▶ sched.Start / StartDelete / StartRestore
                                    │
                                    ▼
                         start(job):  lock system (running[id]) ─ ErrBusy if taken
                                      validate (client has repository, archive belongs to client, …)
                                      store.CreateJob (status "running")
                                      go execute(…) ─▶ runJob ─▶ borg.Service.{Create,Prune,Delete,Restore}
                                                   ─▶ store.FinishJob(status, output, bytes)
                                      release lock, then notify (mail) if NotifyEmail is set
```

Key rules:

- **At most one job per system** at a time (`running map[int64]bool` under a mutex). A restore runs
  as a job of the system the archive belongs to.
- The exit code of borg decides the status: 0 = success, 1 = warning, anything else = failed.
- `checkPlans` moves `next_backup_at` / `next_prune_at` **before** starting the job, so a
  missed run (Locutus was down) is caught up once, not repeatedly.
- On start, `FailRunningJobs` marks jobs that were running when Locutus stopped as failed.
- A job gets a 24-hour timeout and the scheduler context; on shutdown `Wait` gives running jobs
  30 seconds.
- Mailing happens after the system lock is released (a slow SMTP server must not block the next
  job); the result of mailing is appended to the job output ([notify.go](internal/scheduler/notify.go)).

### Interactive borg calls

Listing archives and browsing an archive are not jobs: the controller calls `borg.Service`
directly (`Archives`, `Tree`, `ExtractFile`, `ExportTar`) with a 10-minute timeout. A download is
streamed straight to the response and recorded in the `restores` table afterwards.

## The borg layer

[internal/borg](internal/borg) hides borg behind Go functions. The three main types:

| Type | Meaning |
|------|---------|
| `borg.Repo` | how to reach a repository: location (`/path` or `ssh://user@host:port/path`), SSH key, prune key, passphrase, borg major version |
| `borg.Client` | where `borg create` runs: local (in the container) or remote (address, user, key, sudo) |
| `borg.Service` | runs borg; holds the options, the known_hosts (`remote.HostKeys`) and a small cache of archive trees |

### Driver: command lines per borg version

`borg.Driver` ([borg.go](internal/borg/borg.go)) builds the arguments of every borg command and
parses its output. `v1` ([v1.go](internal/borg/v1.go)) is the only implementation; a borg 2
driver can be added next to it and registered in `DriverFor` and `SupportedVersions`.
`Service` never builds borg arguments itself, it always asks the driver. Archive paths are passed
with the `pp:` pattern style so that file names are never interpreted as patterns.

### Running borg locally

`Service.run` starts `borg` with a fixed, minimal environment (`env()`): `BORG_BASE_DIR`,
`BORG_PASSPHRASE` (always set, so borg never prompts), the "I know what I am doing" answers set to
`NO`, and `BORG_RSH` with `BatchMode` and Locutus' own known_hosts file.

SSH keys **never touch the disk**. For a remote repository, `withAgent` starts an in-memory
`ssh-agent` ([agent.go](internal/borg/agent.go)) on a private unix socket holding the repository
key and passes `SSH_AUTH_SOCK` to borg. The socket is removed when borg exits.

### Backing up a remote client

`Service.Create` for a remote client:

1. Builds the borg command line with the driver and quotes it for the remote shell
   (`remoteCommand`, `ShellQuote`; wildcards in backup paths stay unquoted via `ShellQuoteGlob`, so
   the shell on the client expands them).
2. Prefixes it with `RemoteEnv` (`BORG_PASSPHRASE_FD=0` — the passphrase goes over stdin, never on
   a command line) and optionally `sudo -n`.
3. Logs in with `remote.Run` using the **client** key and forwards an agent holding the
   **repository** key, so borg on the client can reach the repository server itself.

The exact command lines matter: [contrib/locutus-borg-wrapper.sh](contrib/locutus-borg-wrapper.sh)
only allows these forms. If you change a remote command, change the wrapper too and test it
(`TestWrapperRestore`, `TestRemoteCommandGlob`).

### Prune and delete

Both go through `removeAndCompact`: run `borg prune`/`borg delete` with the **prune key** if the
repository has one (`Repo.ForPrune`), then `borg compact`, and drop cached trees of that
repository. The prune key is only used by Locutus itself and never forwarded to a client.

### Restore pipeline

A restore ([restore.go](internal/borg/restore.go)) never gives the client the repository key.
Locutus reads the archive itself and streams files to the target:

```
borg export-tar ──pipe──▶ restorePlan.filter ──pipe──▶ RestoreTarget.Extract (tar -x -p -f - -C /)
                          (drop existing files unless
                           overwrite, protected paths,
                           unsafe names; prefix the
                           destination to every name)
```

1. The selected paths are taken from the cached archive tree (`Tree.Below`).
2. `RestoreTarget.Existing` checks which already exist (remote: `xargs -0 stat` with the paths on
   stdin).
3. A dry run stops here and reports per file what would happen.
4. Otherwise three goroutines are connected with `io.Pipe`; the first error cancels the others.

`RestoreTarget` has two implementations: `localTarget` (the Locutus container; refuses system
directories and `Config.ProtectedPaths()`) and `remoteTarget` (over SSH with two fixed commands,
`RestoreCommands`). File names never appear on a command line.

### SSH and host keys

[internal/remote](internal/remote/remote.go) is a thin SSH client on `golang.org/x/crypto/ssh`.
`HostKeys` implements **trust on first use** against `known_hosts` in the data directory: an
unknown host is added, a changed key is refused. The same file is used by Locutus' own SSH
connections and by the `ssh` that borg starts.

## The Locutus client container

The client container is configured by Locutus, not by its own environment (since v1.2).

```
Locutus (admin changes a system with location "locutus-client")
   │  pushAffected → pushContainer (one at a time per container, pushLocks)
   │  containerConfig: systems in that container → client.Pushed (JSON)
   ▼
ssh borg@host:port  "apply"   (management key; authorized_keys forces sudo locutus-client apply)
   │  stdin: client.Pushed JSON
   ▼
locutus-client apply (as root):  client.Build (validate against Settings) → System.Apply
                                 → writes authorized_keys, sshd_config, sudoers, ssh config
                                 → SaveState in /config
   │  stdout: client.ApplyResult JSON
   ▼
Locutus stores push_ok / push_output in client_containers and shows it
```

- **Settings** (`client.Settings`) come from the container environment: only the management key
  and a few optional values. **Pushed** (`client.Pushed`) is what Locutus sends. **Config** is
  both combined after validation by `client.Build`.
- `client.System` has a `Root` so `Apply` can be tested against a temporary directory
  ([client_test.go](internal/client/client_test.go)).
- The rendering of the configuration files (authorized_keys, sshd, sudoers) is in
  [render.go](internal/client/render.go); keep it in line with the README instructions for a
  hand-configured client, which it automates.
- In the database a container system is stored as `location = 'remote'` with
  `client_container = 1`; `scanSystem`/`SaveSystem` translate that to and from
  `models.LocationLocutusClient`. Code above `models` only sees the three locations.

## Data model and migrations

The whole schema is in [internal/db/db.go](internal/db/db.go). The main tables:

| Table | Holds |
|-------|-------|
| `users`, `sessions`, `auth_states`, `identity_providers` | accounts, logins, pending OIDC/SAML logins |
| `systems` | repositories and clients (`kind`), with encrypted `ssh_key`, `prune_ssh_key`, `passphrase`; a client points to its repository with `repository_id` |
| `system_users` | which users may see which systems |
| `backup_plans` | one row per client: paths, excludes, schedules, retention, next run times, mail addresses |
| `jobs` | history and output of backups, prunes, deletes and restores |
| `restores` | history of downloads |
| `client_containers` | per host:port: the encrypted management key and the result of the last push |
| `settings` | key/value settings of the admin (mail server, log retention) |

Things to know:

- Times are stored as **Unix seconds** (`INTEGER`); `models` converts them to `time.Time`.
- Booleans are `INTEGER` 0/1. Optional foreign keys use `NULL` (see `nullID`).
- SQLite runs with `foreign_keys(1)`, WAL, and **one connection** (`SetMaxOpenConns(1)`) to avoid
  "database is locked". Keep transactions short; never hold a transaction while running borg.
- Secrets are never loaded into `models.System`; it only has `HasSSHKey`, `HasPassphrase`,
  `HasPruneKey`. Decrypted values are only available via `Store.SystemSecrets`, used by the
  scheduler and the push code.

### Migrations

`migrations` is a list of SQL strings; `schema_version` records how many have been applied, and
each one runs in its own transaction. Rules (see also the comment above the list):

- **Within a major version, never change an existing entry; only append a new one.** A 1.x
  database of any earlier minor version must upgrade in one go.
- Write migrations that work on every earlier 1.x database (`ALTER TABLE ... ADD COLUMN ...
  DEFAULT ...`, `CREATE TABLE`), and extend `TestUpgradeFromV10` when you add one.
- A new major version (2.0) starts again from one migration with the complete schema and comes
  with an upgrade script for 1.x databases.

## Security model

Most design decisions in Locutus follow from these rules. Keep them when you change code.

- **Secrets are encrypted at rest** with AES-256-GCM (`secrets.Box`, values prefixed `enc:v1:`).
  Nobody can view them in the interface, not even the admin; the admin can only set or replace
  them. Never render a secret, and never log one.
- **Keys never touch the disk** during use: in-memory ssh-agent for borg, `ssh.ParseRawPrivateKey`
  for Locutus' own connections. Passphrases go over stdin or the environment of a local process,
  never on a command line.
- **Separation of roles**: the admin manages systems but cannot see backups; users only see
  *assigned* systems (`userSystem` → 404 otherwise, so existence is not revealed).
- **Archives belong to clients by prefix** (`<prefix>.<timestamp>`). The scheduler checks that an
  archive to delete or restore starts with the client's prefix.
- **No shell injection**: every argument for a remote shell goes through `ShellQuote` (or
  `ShellQuoteGlob`); restores pass file names on stdin; archive names starting with `-` or
  containing `::` are refused.
- **Web**: CSRF double-submit cookie on every POST, `HttpOnly` + `SameSite=Lax` cookies (`Secure`
  with HTTPS), strict CSP (no inline JavaScript or CSS), safe redirects only to local paths
  (see `setLanguage`), `html/template` auto-escaping.
- **Host keys**: trust on first use; a changed key is always refused until an admin acts.
- **Restores into the container** never write to the data directory, the binary or system
  directories (`Config.ProtectedPaths`, `systemDirs`).

## Views, front end and translations

### Templates

[internal/views](internal/views/views.go) parses every `templates/*.html` page together with
`layout.html` and all partials (`_*.html`) at startup. A page template defines
`{{define "content"}}`, which `layout.html` places in the page; shared pieces are `{{define}}`d
in `_partials.html`.

Every template receives a `*views.Page`: `.User`, `.CSRF`, `.Flash`, `.Errors`, `.Lang`, `.Path`
and the page-specific `.Data`. Template functions: `t` (translate), `date`, `datesec`, `weekday`,
`bytes`, `join`, `hasPrefix`, `hasID`, `add`, `list`, `dict` (pass several values to a
sub-template). Every POST form must contain `<input type="hidden" name="csrf" value="{{.CSRF}}">`.

### JavaScript and CSS

[static/app.js](internal/views/static/app.js) is plain ES5 JavaScript without a framework or a
build step, in one IIFE. Behaviour is attached through `data-*` attributes, never inline handlers
(the CSP forbids them):

- `data-confirm="…"` on a form or submit button asks for confirmation;
- `data-busy="…"` disables the button and shows a spinner while a slow action runs;
- `data-show-when` shows or hides fields depending on another field;
- the schedule editor fetches `/systems/{id}/schedule/preview` for the description and the next runs;
- the archive tree loads one directory at a time as JSON from `/systems/{id}/archives/{archive}/tree`;
- `data-reload="5"` reloads the page after 5 seconds (the page of a running job).

Pages must work without JavaScript as far as possible; JavaScript only improves them.
[style.css](internal/views/static/style.css) is one hand-written stylesheet.

### Translations

[internal/i18n](internal/i18n/i18n.go) loads `locales/*.json`: flat `"key": "text"` maps, with
`%s`/`%d` placeholders for `fmt.Sprintf`. The language comes from the `lang` cookie, then from
`Accept-Language`, then from `LOCUTUS_DEFAULT_LANGUAGE`.

- **All user-facing text goes through a key**, in Go (`a.t(r, "key", args...)`, `a.flash(...)`)
  and in templates (`{{t "key"}}`). Do not put literal English in templates.
- Keys are grouped by page or subject with dots (`admin.logging.intro`, `error.not_found`,
  `container.pushed`) and kept sorted alphabetically in the files.
- `TestCatalogsComplete` fails when `en.json` and `nl.json` do not have exactly the same keys.
  Always add a key to both.
- Adding a language is adding one JSON file with the same keys and a `lang.name`.
- Job output (borg output and the lines Locutus adds to it) is in English on purpose: it is a
  log, also read by people who did not start the job. Mails about a job are translated.

## Design patterns

| Pattern | Where | Why |
|---------|-------|-----|
| **MVC** | `models` / `views` / `controllers` | Clear split between data, presentation and request handling; the package comments say which part each package is. |
| **Dependency injection by constructor** | `main.go` → `controllers.New`, `scheduler.New`, `models.NewStore` | No globals; every dependency is visible in a struct; easy to test parts in isolation. |
| **Single data access object** | `models.Store` | All SQL in one package, with methods per use case (`ListUserSystems`, `FinishJob`, …) instead of a generic ORM. |
| **Middleware / decorator** | `securityHeaders`, `loadSession`, `requireRole`, … | Cross-cutting concerns wrap `http.Handler`/`http.HandlerFunc`. |
| **Strategy** | `borg.Driver` (per borg version), `borg.RestoreTarget` (local / remote) | Swap command lines or targets without touching the flow. |
| **Pipes and filters** | restore pipeline (`io.Pipe` + `restorePlan.filter`) | Stream archives of any size without temporary files. |
| **Background worker with per-key lock** | `Scheduler.start` (`running` map), `pushLocks` | One job per system; one push per container, last change wins. |
| **Cache with TTL and size limit** | `borg.treeCache` | Browsing an archive must not run `borg list` for every directory. |
| **Post/Redirect/Get + flash messages** | controllers | No double submits on reload; messages survive the redirect. |
| **Trust on first use** | `remote.HostKeys` | SSH host keys without manual setup, but changes are refused. |
| **Append-only migrations** | `db.migrations` | Upgrade any 1.x database in one go. |
| **Testable file system root** | `client.System{Root, Chown, LookPath}` | Run the container setup against a temp dir in tests. |

## Code style and conventions

### Go

- **`gofmt`** (tabs, standard formatting). Run `go vet ./...` before committing. No linter
  configuration is checked in; keep `go vet` clean.
- **Every package has a package comment** saying what it is for; every exported identifier has a
  doc comment that starts with its name (`// Start begins a backup ...`). Unexported functions get
  a comment when the *why* is not obvious.
- **Comments explain why**, not what: the reason for a rule, a security consequence, a borg quirk.
  Look at `db.migrations`, `checkCSRF` and `restore.go` for the expected tone.
- **Errors**: return them, wrap with context using `fmt.Errorf("doing x: %w", err)`, compare with
  `errors.Is`. Sentinel errors are package-level `var ErrX = errors.New(...)` (`models.ErrNotFound`,
  `scheduler.ErrBusy`). Error strings are lower case without a trailing period.
- **`context.Context`** is the first parameter of everything that runs borg or SSH; respect
  cancellation. Use `context.WithoutCancel` when work must outlive the HTTP request (see
  `archiveTreeFor`, `notify`).
- **Logging** with the standard `log` package, short lower-case messages with a subject prefix
  (`"scheduler: list plans: %v"`). No structured logging library.
- **Naming**: short receivers (`a *App`, `s *Service`, `st *Store`), Go-style initialisms
  (`ID`, `SSH`, `URL`), constants for every fixed string that is used in more than one place
  (`models.KindClient`, `models.JobBackup`, `models.RoleBackupper`).
- **SQL** is written inline as raw strings with `?` parameters, never with string concatenation of
  values. Column lists that are shared are constants (`systemColumns`, `userColumns`) with a
  matching `scanX` function.
- **Keep it small and explicit**: no reflection, no code generation, no frameworks. A little
  repetition is preferred over an abstraction that hides what happens.
- Line length is not enforced, but stays around 120 characters.

### Text and documentation

- **English** for code, comments, commit messages and documentation; the interface is translated
  into Dutch via `nl.json`.
- **Commit messages**: one line in the imperative, describing the change for a user or operator
  ("Add build.sh to build both images as image files", "Release v1.2 with ...").
- Changes that affect users or operators also update [README.md](README.md) and, if needed, the
  QuickStarts and the comments in the `docker-compose.yml` files.

## Building, running and testing

You need Go (version in [go.mod](go.mod)) or only Docker.

```sh
# Run the tests and checks
go vet ./...
go test ./...

# Without a local Go installation
docker run --rm -v "$PWD":/app -w /app golang:1.26-alpine sh -c 'go vet ./... && go test ./...'

# Run Locutus locally (outside Docker); borg and ssh must be installed
LOCUTUS_DATA_DIR=./data go run .        # http://localhost:2224

# Build and run the image
docker compose up -d --build

# Build image files for other machines (see README, "Building for another machine")
./build.sh
```

Notes on the tests:

- Tests are plain `testing` table tests, next to the code (`*_test.go` in the same package, so
  they can test unexported functions). There is no test framework or mocking library.
- Tests that need external programs skip when they are missing (`TestRestoreLocal` needs `borg`
  and `tar`). Run the full set in an environment with borg, such as the builder image with
  `apk add borgbackup tar`.
- `TestTemplatesParse` parses all templates and `TestCatalogsComplete` checks the translations:
  run the tests after every template or text change.
- There are no HTTP end-to-end tests. After a change in controllers or templates, click through
  the affected pages with both an admin and a backupper account, in both languages.

## Recipes for common changes

### Add a page or action

1. Add the route in `App.Routes()` with the right role wrapper (`admin`, `view`, `restore`,
   `backup`).
2. Write the handler in the matching file in `controllers`. For `/systems/{id}/...` start with
   `a.userSystem` (or `planClient`/`loadPlan`). POST: validate, act, `flash`, redirect.
3. Add the template in `views/templates/<name>.html` (include the `csrf` field in forms).
4. Add every text as a key in **both** `en.json` and `nl.json`.
5. `go test ./...`, then try it in the browser.

### Add a column or table

1. Append a new migration to `db.migrations` (never edit an earlier one) with a comment naming
   the version.
2. Extend the struct, the column constant, the `scanX` function and the `Save`/`Update` methods in
   `models`.
3. Extend `TestUpgradeFromV10` if the upgrade path needs checking.

### Add an environment variable

Add the field to `config.Config` and read it in `config.Load` with `env("NAME", default)` (the
`LOCUTUS_` prefix is added for you). Document it in the README settings table and in the comments
of `docker-compose.yml`.

### Add or change a borg command

1. Add the method to the `Driver` interface and implement it in `v1`.
2. Use it from a `Service` method; never build borg arguments outside the driver.
3. If the command runs on a remote client, update `contrib/locutus-borg-wrapper.sh` and the sudo
   rules in the README and in `client/render.go`, and mention in the release notes that the
   wrapper must be updated.
4. Add a test of the arguments in `borg_test.go` (`TestV1Args`).

### Release a new version

1. Set `Version` in [internal/version/version.go](internal/version/version.go) (both images get
   it; `build.sh` reads it from there).
2. Add an *Updating to vX.Y* section in the README when operators have to do something.
3. Check that migrations are append-only and that the wrapper and the client container are in
   line with Locutus.
4. Commit as "Release vX.Y …" and build the images (`build.sh` or `docker buildx`).
