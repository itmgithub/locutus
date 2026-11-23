# Locutus

> *Sero locutus, cito paenituit.*

Locutus is a web interface for [BorgBackup](https://www.borgbackup.org/). It lets you manage your
backups from the browser:

- back up servers and directories on the Docker host, on a schedule or right away;
- have old backups cleaned up automatically according to retention rules;
- browse backups and restore files: as a download, directly onto a server, or into a directory
  on the Docker host;
- decide per user which systems they see and what they may do.

Locutus runs as a single Docker container on amd64 and arm64 and works with borg 1.x. The
interface is available in English and Dutch.

## Note
Locutus is currenty being tested. So far it has been working great at a small site with different backup schedules etc. The SAML connection is completely untested! OIDC has been tested but also in a limited scope. Again, backups and restores work great and I found no issues. That said; testing is key!

## Contents

1. [How Locutus works](#how-locutus-works)
2. [Installation](#installation)
3. [Settings](#settings)
4. [Users and roles](#users-and-roles)
5. [Adding a repository](#adding-a-repository)
6. [Adding a client](#adding-a-client)
7. [The Locutus client container](#the-locutus-client-container)
8. [Making backups](#making-backups)
9. [Restoring files](#restoring-files)
10. [Mailing the output of jobs](#mailing-the-output-of-jobs)
11. [Cleaning up logging](#cleaning-up-logging)
12. [Backing up Locutus itself](#backing-up-locutus-itself)
13. [Troubleshooting](#troubleshooting)
14. [Security tips](#security-tips)

## How Locutus works

Everything you work with in Locutus is a **system**. There are two kinds:

- A **repository** is where borg stores the backups: a directory on a backup server (*remote*,
  over SSH), a directory on the Docker host (*local*), or a directory in a
  [Locutus client container](#the-locutus-client-container) (*Locutus client*).
- A **client** is a machine or directory that is backed up. Each client backs up to one
  repository. A *remote* client is a server that Locutus reaches over SSH. A *local* client is a
  directory of the Docker host that you have mounted into the container. A *Locutus client* is a
  machine with Docker on which the Locutus client container runs.

Some examples:

| You want to back up… | to… | Then you create |
|----------------------|-----|-----------------|
| a web server | a backup server | a remote client and a remote repository |
| directories on the Docker host | a backup server | a local client and a remote repository |
| directories on the Docker host | a disk in the Docker host | a local client and a local repository |
| the shares of a NAS with Docker | the NAS itself or another machine with Docker | a client and a repository, both with location *Locutus client* |

A remote client can only back up to a remote repository, because the client itself has to reach
the repository.

The first time, it goes like this:

1. [Install](#installation) Locutus and create the administrator.
2. As the administrator, create a [repository](#adding-a-repository) and a
   [client](#adding-a-client).
3. Create [users](#users-and-roles) and assign the systems to them.
4. A backupper sets up the [backup schedule](#making-backups) of the client.

## Installation

All you need is Docker with Docker Compose. Unpack the Locutus files (or clone the repository),
adjust `docker-compose.yml` if needed (see [Settings](#settings)) and start it:

```sh
docker compose up -d --build
```

Then open `http://<host>:2224`. On the first start you create an administrator; you choose the
username yourself.

At the top right of every page you choose the language. By default Locutus follows the language
of your browser. The version and build number are shown at the bottom of every page, for example
`Locutus v1.2 (Build: abcde) · Sero locutus, cito paenituit.`

### Updating

Get the new version and start again with `docker compose up -d --build`. With a new version, also
check whether the [wrapper on the clients](#5-optional-install-the-locutus-wrapper) needs to be updated.

Within a major version you can update directly from any earlier version, for example from v1.0
to v1.2 without installing v1.1 first. Locutus updates the database itself on the first start;
make a [backup of the data directory](#backing-up-locutus-itself) before updating. A new major
version (such as v2.0) comes with an upgrade script that you run once first; its release notes
say how.

**Updating to v1.2:**

- Locutus now listens on port `2224` instead of `3000`. Use `2224:2224` under `ports:` and
  `LOCUTUS_LISTEN_ADDR=:2224`, as in the new `docker-compose.yml`, or keep the old port by
  setting both back to `3000`.
- The [Locutus client container](#the-locutus-client-container) is now configured by Locutus
  and uses port `2223` by default. Remove the `LOCUTUS_REPOSITORY_...` and `LOCUTUS_CLIENT_...`
  settings from its `docker-compose.yml`, give its systems in Locutus the location *Locutus
  client* and set `LOCUTUS_MANAGER_KEY`, as described in its
  [QuickStart](client/QuickStart.md). If you change the port of an existing container, also
  change it for its systems in Locutus.

### Building for another machine

Want to build Locutus on one machine and run it on another, for example a Raspberry Pi? Then
create image files with `build.sh`. By default it builds both Locutus and the
[Locutus client container](#the-locutus-client-container), for amd64 and arm64:

```sh
./build.sh                       # locutus-amd64.tar, locutus-arm64.tar,
                                 # locutus-client-amd64.tar, locutus-client-arm64.tar
./build.sh -t client -a arm64    # only the client, only arm64
./build.sh -h                    # all options
```

Copy the right file to the target machine and load it with, for example,
`docker load -i locutus-arm64.tar`. The images are tagged `latest` and with the version, such as
`itmon.nl/locutus:v1.2` and `itmon.nl/locutus-client:v1.2`.

By default the build number is the start of the git commit. You can also set it yourself with
`./build.sh -b abcde` or `docker compose build --build-arg BUILD=abcde`.

### Pushing the images to a registry

The repository builds two images, each pushed to its own repository in your registry: Locutus
itself, and the [Locutus client container](#the-locutus-client-container) (the `client` target
of the same Dockerfile). Both have the same version, set in `internal/version/version.go`:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
    -t registry.example.com/locutus:v1.2 --push .
docker buildx build --platform linux/amd64,linux/arm64 --target client \
    -t registry.example.com/locutus-client:v1.2 --push .
```

Then use `image: registry.example.com/locutus:v1.2` in `docker-compose.yml` instead of `build:`.

## Settings

All settings are in `docker-compose.yml`.

### Directories

Locutus keeps its data in the data directory, by default `./data` next to `docker-compose.yml`.
Certificates go into `./certs`, and files you [restore into Locutus](#restoring-into-locutus)
into `./restore` (in the container: `/restore`). To use other directories, put them in a `.env`
file next to `docker-compose.yml`:

```sh
LOCUTUS_HOST_DATA_DIR=/srv/locutus/data
LOCUTUS_HOST_CERT_DIR=/srv/locutus/certs
LOCUTUS_HOST_RESTORE_DIR=/srv/locutus/restore
```

Other directories Locutus needs are mounted under `volumes:`:

```yaml
    volumes:
      - /srv/borg:/repos          # local repositories, for example /repos/myrepo
      - /:/host:ro                # files of the Docker host, for local clients
```

In Locutus you then always use the path in the container, such as `/repos/myrepo` or
`/host/etc`.

### Locutus settings

These go under `environment:`. Usually you only need `TZ` and possibly `LOCUTUS_BASE_URL` and
HTTPS.

| Setting | Default | Meaning |
|---------|---------|---------|
| `TZ` | – | Time zone for schedules and dates, for example `Europe/Amsterdam`. |
| `LOCUTUS_BASE_URL` | – | The address users open Locutus at, for example `https://locutus.example.com`. Needed for logging in through OIDC or SAML and behind a reverse proxy. |
| `LOCUTUS_TLS_CERT`, `LOCUTUS_TLS_KEY` | – | Certificate and key for HTTPS (see below). |
| `LOCUTUS_TLS_CA` | – | Intermediate certificates for the certificate, when they are not already in the certificate file. |
| `LOCUTUS_DEFAULT_LANGUAGE` | `en` | Language for users whose browser asks for neither English nor Dutch: `en` or `nl`. |
| `LOCUTUS_EXTRA_CA_CERTS` | – | Extra trusted certificates for logging in through OIDC or SAML and for the [mail server](#mailing-the-output-of-jobs). See [Certificate error when logging in through OIDC or SAML](#certificate-error-when-logging-in-through-oidc-or-saml). |
| `LOCUTUS_SECRET_KEY` | – | Key with which Locutus stores SSH keys and passphrases encrypted (32 bytes, base64). Without it, Locutus creates `secret.key` in the data directory. |
| `LOCUTUS_LISTEN_ADDR` | `:2224` | Port inside the container. For another port on the host, change `ports:` instead. |

For special cases you can also move parts of the data directory: `LOCUTUS_DATA_DIR` (the data
directory in the container, `/data`), `LOCUTUS_DB_PATH` (the database),
`LOCUTUS_SECRET_KEY_FILE`, `LOCUTUS_KNOWN_HOSTS_FILE` and `LOCUTUS_BORG_BASE_DIR`.

### HTTPS

Put the certificate and the key (PEM) in the certificates directory and set in
`docker-compose.yml`:

```yaml
      - LOCUTUS_TLS_CERT=/certs/locutus_cert.pem
      - LOCUTUS_TLS_KEY=/certs/locutus_key.pem
```

Without these two settings Locutus uses plain HTTP. That is fine behind a reverse proxy that
handles HTTPS.

### Logging in through OIDC or SAML

Besides users with a password in Locutus, people can log in through an identity provider such as
Keycloak, Authentik, Microsoft Entra ID or Google. First set `LOCUTUS_BASE_URL`. Then, as the
administrator, go to **Identity providers**:

- **OIDC**: enter the issuer URL, the client ID and the client secret. Register the redirect URL
  shown in the form with the provider.
- **SAMLv2**: enter the metadata URL or the metadata XML of the provider. After saving, Locutus
  shows the metadata URL to enter at the provider.

Someone who logs in this way for the first time automatically gets an account without a role.
They can do nothing until the administrator assigns a role.

## Users and roles

The administrator creates users under **Users**, assigns roles, and under **Systems** decides per
system which users see it.

| Role | Can |
|------|-----|
| **Admin** | Manage systems, users, roles, identity providers and the [mail server](#mailing-the-output-of-jobs), and [clean up logging](#cleaning-up-logging). Does *not* see backups or files. |
| **Viewer** | Browse the backups of assigned systems, including the files in them, and see what has happened. |
| **Restorer** | Everything a viewer can, plus [download or restore](#restoring-files) files. |
| **Backupper** | Everything a restorer can, plus set up the schedules of assigned clients, start a backup or prune right away, and delete backups. |

For an assigned client you only see the backups of that client. For an assigned repository you
see all backups in that repository.

Nobody can view SSH keys and passphrases, not even the administrator. The administrator can only
set or replace them.

## Adding a repository

As the administrator, go to **Systems › New repository**. If the backup server runs Docker, the
[Locutus client container](#the-locutus-client-container) can also be the backup server.

### Local repository

Choose *Local* and enter the path in the container, for example `/repos/myrepo` (see
[Directories](#directories)). Save and click **Initialise** to create a new, empty repository.

### Remote repository on a backup server

Choose *Remote* and enter the host, the SSH user and the path. Save and click **Generate new key
pair**. Locutus then shows the public key.

On the backup server:

```sh
apt install borgbackup             # or: apk add borgbackup / dnf install borgbackup
useradd -m -s /bin/sh borg
install -d -o borg -m 700 /srv/borg ~borg/.ssh
```

Put the public key in `~borg/.ssh/authorized_keys`, restricted to borg:

```
command="borg serve --restrict-to-path /srv/borg",restrict ssh-ed25519 AAAA... locutus
```

Then click **Test connection** in Locutus and, for a new repository, **Initialise**.

### Encryption

Enter a **passphrase** before initialising the repository and choose an encrypted mode
(`repokey-blake2` is a good choice). Keep the passphrase outside Locutus as well, together with an
export of the repository key (`borg key export`). Without these two you cannot get to your
backups if Locutus is lost.

### Extra protection: append-only

With *append-only*, the key cannot really delete existing backups. If a client is hacked, an
attacker then cannot destroy your old backups. Because cleaning up old backups must still be
possible, you then use two keys:

- the **normal key**, append-only, for backups and restores;
- the **prune key**, without append-only, which only Locutus itself uses for cleaning up and
  deleting. Clients never get this key.

Click **Generate prune key pair** on the repository and put both keys on the backup server, each
on its own line:

```
# Normal key
command="borg serve --append-only --restrict-to-repository /srv/borg/repo",restrict ssh-ed25519 AAAA... locutus

# Prune key: only from the IP address of Locutus
from="192.0.2.10",command="borg serve --restrict-to-repository /srv/borg/repo",restrict ssh-ed25519 AAAA... locutus-prune
```

- At `from=`, enter the IP address Locutus uses to reach the backup server. Usually that is the
  address of the Docker host. Separate multiple addresses with commas.
- With several repositories, give each its own line with its own path.

Then click **Test connection** (both keys are tested) and run **Prune now** once on a client. The
output then says `Using the separate prune key of the repository.`

Note: the first prune with the prune key also makes earlier deletions final, including those of
an attacker. So don't prune more often than needed, for example weekly, and check now and then
that the list of backups looks normal. When in doubt, see *Append-only mode* in the borg
documentation.

## Adding a client

As the administrator, go to **Systems › New client** and choose the repository the client backs
up to. To back up directories of a machine that runs Docker, such as a server or a NAS, you can
also use the [Locutus client container](#the-locutus-client-container).

### Local client

Choose *Local*. The client backs up directories you have mounted into the container, for example
`/host/etc` and `/host/home` (see [Directories](#directories)). Nothing else needs to be set up.

### Remote client

Choose *Remote* and enter the host and the SSH user, for example `borg`. Tick **Run borg and
restores with sudo** if Locutus has to back up or restore files that this user cannot read or
write (see [step 3](#3-optional-set-up-sudo)). Save and click **Generate new key pair**. Then set
up the client as described below.

During a backup, Locutus logs in on the client and temporarily gives it access to the repository
key. Borg on the client uses it to connect to the backup server itself. So the client needs no
repository key of its own, but it must be able to reach the backup server. For a restore, Locutus
reads the files from the repository itself and sends them to the client.

Locutus never needs to log in as root.

#### 1. Install borg and create a user

```sh
apt install borgbackup
useradd -m -s /bin/sh borg
```

#### 2. Allow the Locutus key

Put the public key of the client (see Locutus) in `~borg/.ssh/authorized_keys`:

```
restrict,agent-forwarding ssh-ed25519 AAAA... locutus
```

`restrict` turns off everything that is not needed; `agent-forwarding` turns back on the one
thing Locutus needs. To limit the key further to exactly the commands of Locutus, use the
[wrapper](#5-optional-install-the-locutus-wrapper).

To lock down sshd further for this user, add this to `/etc/ssh/sshd_config` and restart sshd:

```
Match User borg
    AllowAgentForwarding yes
    AllowTcpForwarding no
    X11Forwarding no
    PermitTTY no
    PasswordAuthentication no
```

#### 3. Optional: set up sudo

Without sudo, borg runs as the user `borg`. It can then only back up files that user can read, and
only restore files where that user can write. To back up and restore everything, tick **Run borg
and restores with sudo** in Locutus and allow these commands in sudo. Locutus then runs:

- `sudo -n borg ...` for backups and for **Test connection**;
- `sudo -n stat ...` and `sudo -n tar -x -p -f - -C /` for restores.

Setting up sudo is done on the client itself. Create the file with
`visudo -f /etc/sudoers.d/locutus`:

```
# Backups: borg as root. The variables carry the passphrase and the
# repository key through sudo.
Defaults!/usr/bin/borg env_keep += "BORG_PASSPHRASE_FD BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK BORG_RELOCATED_REPO_ACCESS_IS_OK SSH_AUTH_SOCK"
borg ALL=(root) NOPASSWD: /usr/bin/borg

# Restores (leave this out if nothing may be restored to this client)
borg ALL=(root) NOPASSWD: /usr/bin/stat, /usr/bin/tar -x -p -f - -C /
```

- Check the paths with `command -v borg stat tar` and adjust them if needed. On Alpine, for
  example, they are `/bin/stat` and `/bin/tar`.
- Use the name of your SSH user instead of `borg` if it is different.
- Check the result as that user with `sudo -n -l`.

Keep in mind that someone who can log in as this user can then also run borg as root, and borg
can read and write any file. Only let Locutus log in as this user, and preferably use the
[wrapper](#5-optional-install-the-locutus-wrapper).

#### 4. Introduce the backup server to the client

With sudo, borg connects to the backup server as root, so run this as root. Without sudo, run it as
the user `borg`.

```sh
ssh-keyscan -p 22 repo.example.com >> ~/.ssh/known_hosts
ssh-keygen -lf ~/.ssh/known_hosts     # compare the fingerprint with that of the backup server
```

Then create `~/.ssh/config` (again as root with sudo, as `borg` without), with the same host name
as the repository in Locutus:

```
Host repo.example.com
    User borg
    Port 22
    IdentitiesOnly no
    IdentityAgent SSH_AUTH_SOCK
    ForwardAgent no
    ForwardX11 no
    ClearAllForwardings yes
    BatchMode yes
    StrictHostKeyChecking yes
    UpdateHostKeys no
    ServerAliveInterval 30
    ServerAliveCountMax 6
```

#### 5. Optional: install the Locutus wrapper

Without the wrapper, the Locutus key can run any command as the user `borg`. The wrapper makes
sure the key can only make backups and restore files, and nothing else. Copy
[`contrib/locutus-borg-wrapper.sh`](contrib/locutus-borg-wrapper.sh) to the client, install it:

```sh
install -o root -g root -m 755 locutus-borg-wrapper.sh /usr/local/bin/locutus-borg-wrapper
```

and put it in front of the key in `~borg/.ssh/authorized_keys`:

```
command="/usr/local/bin/locutus-borg-wrapper",restrict,agent-forwarding ssh-ed25519 AAAA... locutus
```

If you don't want files to be restored to this client, use
`command="/usr/local/bin/locutus-borg-wrapper --no-restore"`.

After updating Locutus, install the new wrapper too. An old wrapper may refuse new commands.

#### 6. Test

Click **Test connection** on the client. You then see the borg version on the client, for example
`borg 1.4.0`. With sudo, this also shows that sudo works for borg. Then make a first backup (see
[Making backups](#making-backups)) and check the output.

## The Locutus client container

The Locutus client is a container for any machine that runs Docker: a server, a NAS, a
Raspberry Pi, or a host whose other containers keep their data in volumes. Locutus reaches
it over SSH, and it can be:

- a **repository server**: it serves borg repositories from a directory mounted at `/repos`;
- a **client**: Locutus backs up the directories mounted below `/volumes` and restores files to
  them;
- or both at the same time.

Locutus configures the container itself. In Locutus, you give the repositories and the client
the location **Locutus client**, with the host and port of the container. Locutus then pushes the
configuration over SSH: the repositories with their keys, the client key and the host keys of the
repository servers. The container sets up the user `borg`, sshd, the wrapper and sudo in the same
way as described above for a remote repository and a remote client, including append-only with a
prune key.

The `docker-compose.yml` of the container only holds which directories to mount and the
**management key** (`LOCUTUS_MANAGER_KEY`): the public key with which Locutus pushes the
configuration. That key can do nothing else: it is restricted to one command that reads the
configuration, checks it and applies it. Optionally, `LOCUTUS_MANAGER_FROM` limits it (and the
prune keys) to the address of Locutus.

Locutus pushes the configuration automatically after every change of a system in the container
and before **Test connection**, and with the **Push configuration** button. The container keeps
the pushed configuration, its SSH host key and the borg cache in `/config`. The repositories
themselves are in `/repos`, outside the container. So you can throw the container away and start
a new one, for example with a new version:

- **with the same `/config`**: everything works right away;
- **without `/config`**: the container has a new host key. Locutus then refuses the connection
  until you click **Check host key**, compare the fingerprint with the log of the container and
  accept it. Locutus then pushes the configuration again, and the existing repositories in
  `/repos` are served as before.

See [client/QuickStart.md](client/QuickStart.md) for the setup and
[client/docker-compose.yml](client/docker-compose.yml) for all settings. Use the same version of
the client container as of Locutus.

## Making backups

This is done by a backupper. Open **My systems** and choose the client.

### The backup schedule

Click **Backup schedule**:

- **Files and directories**: one path per line, for example `/etc` and `/home`. Wildcards are
  allowed, such as `/home/*/Documents`. As in the shell, files starting with a dot are not
  included; use for example `/home/*/.*` for those.
- **Exclude**: one pattern per line, for example `/home/*/.cache` or `*.tmp`.
- **Compression**: `lz4` is fast, `zstd` makes smaller backups.
- **When**: choose a frequency. Locutus immediately shows a description and the next five times
  the backup runs.

| Frequency | Example |
|-----------|---------|
| Every few minutes | every 15 minutes |
| Every hour or every few hours | every 6 hours at 12 minutes past the hour |
| Daily | every day at 04:33 |
| Weekly | Monday, Wednesday and Friday at 22:00 |
| Monthly | on day 1 and 15 at 02:05 |
| Custom cron expression | anything a crontab can do, such as `15 8-18 * * 1-5` |

Tick **Run backups according to this schedule** and save. **Back up now** starts a backup right
away. Times follow the time zone `TZ`.

### Cleaning up old backups (prune)

Click **Prune schedule** and set how many backups per period are kept. By default that is 24
hourly, 7 daily, 4 weekly and 6 monthly. Choose when the cleanup runs, tick the schedule and save.
**Prune now** cleans up right away.

Cleanup works per client: only the backups of that client are considered.

### Deleting a backup

Click **Delete** next to the backup. After confirmation, the backup is removed from the
repository and the space is freed. This cannot be undone.

### Good to know

- One task runs at a time per system: a backup, prune, deletion or restore.
- If Locutus was off when a backup or prune should have run, it is caught up once after starting.
- The backups of a client are named `<name>.<date>T<time>`. The first part comes from the name of
  the client when it was created, and does not change if you rename the client later.
- Under **Logging** on the page of the client you see which tasks have run, with the output of
  borg.

## Restoring files

This is done by a restorer or backupper. Open a backup of a system with **View / restore** and
tick the files and directories you want back. A ticked directory includes everything in it.

### Downloading

Click **Download**. A single file is downloaded as is; several files or directories come as a
`.tar.gz` file.

### Restoring to a client or into Locutus

Below the files, fill in:

1. **To client**: where the files go. By default this is the client the backup was made of. You
   can choose from the clients assigned to you, or *Local (Locutus)*.
2. **Destination**: the directory the files go into. They keep their path from the backup below
   that directory: with destination `/restore`, `/etc/hosts` ends up in `/restore/etc/hosts`.
   Choose `/` to restore files to their original place.
3. **Dry run** (on by default): nothing is written yet. You see per file where it would go and
   whether it is new, skipped or overwritten.
4. **Overwrite existing files** (off by default): when off, files that already exist at the
   destination are left alone. When on, they are replaced by the version from the backup.

Click **Start dry run**, check the result, go back, untick Dry run and click **Start restore**.
You see the progress on the page of the restore. Afterwards the restore is also listed under
**Logging: restores and downloads** on the system.

Files get the same permissions and dates as in the backup, and also the same owner when the
client uses [sudo](#3-optional-set-up-sudo).

On a remote client the files are written as the SSH user, or as root with sudo. The client needs
`tar` and `stat`, which are almost always installed. The client does not need to reach the backup
server.

### Restoring into Locutus

With *Local (Locutus)* or a local client, the files go into the container. Use `/restore` as
destination. The files then end up on the Docker host in `./restore` next to
`docker-compose.yml`, or in `LOCUTUS_HOST_RESTORE_DIR` (see [Directories](#directories)).

The directories of Locutus itself, such as the data directory, are protected. Locutus never
restores anything there.

## Mailing the output of jobs

Locutus can mail the output of backups, prunes and restores, the same output you see under
**Logging**. This is optional.

### Setting up the mail server

As the administrator, go to **Mail** and fill in:

- **Host** and **Port** of the mail server;
- **Connection security**: *STARTTLS* (usually port 587) or *SMTPS* (usually port 465). The
  connection is always encrypted: if the server does not offer STARTTLS, nothing is sent. The
  certificate of the mail server is checked; for an internal CA, see `LOCUTUS_EXTRA_CA_CERTS`;
- **Sender address**, for example `locutus@example.com`;
- optionally a **Username** and **Password**. The password is stored encrypted and cannot be
  viewed; leave the field empty to keep it.

Save and click **Send test mail**. To stop mailing, empty the host and save.

### Who gets the mail

- **Scheduled backups and prunes**: fill in **Mail the output to** in the backup schedule and
  in the prune schedule. Separate several addresses with commas. Leave it empty for no mail.
- **Back up now and Prune now**: the arrow next to the button opens a field with your own email
  address filled in. Change or empty it before clicking the button. Without opening it, the
  output is mailed to your own address.
- **Restores**: the same field is below the restore options.
  Local users set their address under **Account**; users of an identity provider get it from
  the provider.

The mail is sent when the job has finished. Whether that worked is added to the end of the
output of the job, for example `The output has been mailed to ops@example.com.` The mail is in
the language of the person who started the job; scheduled jobs use
`LOCUTUS_DEFAULT_LANGUAGE`.

## Cleaning up logging

For every system, Locutus records which backups, prunes, deletions, restores and downloads have
run, with the output of borg. That is the logging. It grows, so the administrator can clean it up
under **Logging**. There are three types:

| Type | Contains |
|------|----------|
| Backup jobs | scheduled and manual backups |
| Restore jobs | restores and downloads |
| Other logging | prunes and deleted backups |

- **Automatically**: enter per type how many days of logging are kept and click **Save**. Older
  logging is deleted right away, and every hour after that. 0 means keep forever; that is the
  default.
- **By hand**: choose a date (by default a week ago) and one or more types, and click **Delete**.
  All logging of those types from before that date is deleted.

The backups themselves are always kept. A task that is still running is not deleted.

## Backing up Locutus itself

If you lose the data directory of Locutus, you also lose the stored SSH keys and passphrases. So
make a backup of it. The important parts are in the data directory:

| What | Why |
|------|-----|
| `locutus.db`, `locutus.db-wal`, `locutus.db-shm` | Users, systems, schedules and logging. Always back up together. |
| `secret.key` | **Without this file the stored SSH keys and passphrases are useless.** If you use `LOCUTUS_SECRET_KEY`, keep that value somewhere safe instead. |
| `ssh/known_hosts` | The known servers. |
| `borg/.config/` | Borg's data about the repositories. |

You can do this with Locutus itself: create a local client with these paths and let it back up to
a **remote, encrypted** repository.

```
/data/locutus.db*
/data/secret.key
/data/ssh
/data/borg/.config
```

Because the database changes while Locutus runs, such a backup is very occasionally not entirely
consistent. For certainty, make the backup from the host while Locutus is briefly stopped
(`docker compose stop`, back up the data directory, `docker compose start`).

**Restoring**: stop Locutus with `docker compose down`, put the files back in the data directory
(with `borg extract` or with another Locutus installation) and start with `docker compose up -d`.

Also keep the passphrases of your repositories and an export of the repository keys
(`borg key export`) in a safe place outside Locutus.

## Troubleshooting

### Connection refused: host key changed

Locutus remembers the host key of every server on the first connection. If it changes later, for
example after a reinstall, Locutus refuses the connection. If the change is expected, remove the
line of that server from `ssh/known_hosts` in the data directory and test the connection again.
For a [Locutus client container](#the-locutus-client-container), click **Check host key** on one
of its systems instead.

### Restoring to a client is refused

If you see *the client refused the restore*, the client uses the wrapper: install the current
[wrapper](#5-optional-install-the-locutus-wrapper) and check that `authorized_keys` does not
contain `--no-restore`.

### sudo: a password is required

The client is set up with sudo in Locutus, but sudo does not allow the command. Check
`/etc/sudoers.d/locutus` (see [Set up sudo](#3-optional-set-up-sudo)), especially the paths of
borg, stat and tar, and run `sudo -n -l` as the SSH user.

### Permission denied in a backup or restore

The SSH user cannot read the files of the backup, or cannot write at the destination of the
restore. Use [sudo](#3-optional-set-up-sudo), or choose files and a destination the user has access
to.

### A destination is refused

When restoring into Locutus, the directories of Locutus itself are protected. Choose a mounted
directory, such as `/restore`.

### No logging, but there are backups

If a client shows *no logging* while there are backups, the administrator has cleaned up the
logging. The backups themselves are still there.

### Certificate error when logging in through OIDC or SAML

If logging in fails with `x509: certificate signed by unknown authority`, Locutus does not trust
the certificate of the identity provider. This usually happens because:

1. **the server of the identity provider sends an incomplete certificate chain** (common with
   Let's Encrypt). Browsers fill this in themselves, Locutus does not. Check it like this:
   ```sh
   echo | openssl s_client -connect idp.example.com:443 -servername idp.example.com -showcerts | grep -E ' s:| i:|Verify return'
   ```
   If it says `unable to verify the first certificate`, the intermediate certificate is missing.
   The best fix is to let the server use `fullchain.pem` instead of `cert.pem`.
2. **the identity provider uses its own (internal) CA.**

In both cases you can let Locutus trust the missing certificate. Put it as a PEM file in the `ca`
directory inside the certificates directory and set in `docker-compose.yml`:

```yaml
      - LOCUTUS_EXTRA_CA_CERTS=/certs/ca
```

For Let's Encrypt you find the intermediate certificate at <https://letsencrypt.org/certificates/>.
After a restart, the Locutus log says `trusting N extra CA certificate(s)`. Intermediate
certificates expire, so treat this as a temporary fix and repair the chain on the server.

## Security tips

- Use HTTPS when Locutus can be reached from outside a trusted network, either directly or
  through a reverse proxy.
- Restrict the keys on the backup server to `borg serve`, preferably
  [append-only with a separate prune key](#extra-protection-append-only).
- During a backup, someone with root access on the client can use the repository key. So give
  groups of clients that must not see each other's backups their own repository each.
- Let Locutus log in as an ordinary user, never as root. Use [sudo](#3-optional-set-up-sudo)
  when it needs access to all files, and preferably the
  [wrapper](#5-optional-install-the-locutus-wrapper) to limit the key to what Locutus does.
- A restorer can restore files to assigned clients and, with *overwrite*, replace existing files,
  including in for example `/etc` when the client uses sudo. Only assign clients to restorers you
  trust with that, or leave the restore rule out of sudoers, or use `--no-restore` in the
  wrapper.
- Keep the passphrases and key exports of your repositories, and `secret.key`, outside Locutus
  as well.
