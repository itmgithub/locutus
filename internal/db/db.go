// Package db opens the SQLite database and applies schema migrations.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure Go SQLite driver (no CGO)
)

// Open opens (and creates if needed) the SQLite database at path and
// brings the schema up to date.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows a single writer; one connection avoids "database is locked" errors.
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		database.Close()
		return nil, err
	}
	// The database holds (encrypted) secrets: keep it private.
	if err := os.Chmod(path, 0o600); err != nil {
		database.Close()
		return nil, err
	}
	if err := migrate(database); err != nil {
		database.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return database, nil
}

// migrations are applied in order; schema_version records how many have
// been applied. A database of any earlier 1.x version is therefore brought up
// to date in one go, also when versions in between were skipped.
//
// Within a major version, never change an existing entry, only append. A new
// major version starts again from a single entry with the complete schema;
// databases of the previous major version are converted by an upgrade script
// first, so this list does not keep growing.
var migrations = []string{
	`
CREATE TABLE identity_providers (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	type       TEXT    NOT NULL CHECK (type IN ('oidc', 'saml')),
	name       TEXT    NOT NULL,
	enabled    INTEGER NOT NULL DEFAULT 1,
	config     TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

CREATE TABLE users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT    NOT NULL,
	display_name  TEXT    NOT NULL DEFAULT '',
	email         TEXT    NOT NULL DEFAULT '',
	password_hash TEXT    NOT NULL DEFAULT '',
	provider_id   INTEGER REFERENCES identity_providers (id) ON DELETE CASCADE,
	subject       TEXT    NOT NULL DEFAULT '',
	role          TEXT    NOT NULL DEFAULT '' CHECK (role IN ('', 'admin', 'viewer', 'restorer', 'backupper')),
	created_at    INTEGER NOT NULL,
	last_login_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX users_local_username ON users (username) WHERE provider_id IS NULL;
CREATE UNIQUE INDEX users_external_subject ON users (provider_id, subject) WHERE provider_id IS NOT NULL;

CREATE TABLE sessions (
	token      TEXT    PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);

CREATE TABLE auth_states (
	state       TEXT    PRIMARY KEY,
	provider_id INTEGER NOT NULL REFERENCES identity_providers (id) ON DELETE CASCADE,
	nonce       TEXT    NOT NULL DEFAULT '',
	request_id  TEXT    NOT NULL DEFAULT '',
	expires_at  INTEGER NOT NULL
);

-- A system is either a borg repository or a client that is backed up.
-- ssh_key and passphrase are encrypted and only ever handled by the admin.
CREATE TABLE systems (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT    NOT NULL UNIQUE COLLATE NOCASE,
	kind          TEXT    NOT NULL CHECK (kind IN ('repository', 'client')),
	location      TEXT    NOT NULL CHECK (location IN ('local', 'remote')),
	description   TEXT    NOT NULL DEFAULT '',
	host          TEXT    NOT NULL DEFAULT '',
	port          INTEGER NOT NULL DEFAULT 22,
	ssh_user      TEXT    NOT NULL DEFAULT '',
	ssh_key       TEXT    NOT NULL DEFAULT '',
	-- repositories only: optional separate key for prune, delete and compact,
	-- so ssh_key can be restricted to "borg serve --append-only"
	prune_ssh_key TEXT    NOT NULL DEFAULT '',
	repo_path     TEXT    NOT NULL DEFAULT '',
	passphrase    TEXT    NOT NULL DEFAULT '',
	borg_version  INTEGER NOT NULL DEFAULT 1,
	repository_id INTEGER REFERENCES systems (id) ON DELETE RESTRICT,
	archive_prefix TEXT   NOT NULL DEFAULT '',
	-- remote clients only: run borg (and tar and stat for restores) with
	-- "sudo -n", so the SSH user does not have to be root
	use_sudo      INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);

CREATE TABLE system_users (
	system_id INTEGER NOT NULL REFERENCES systems (id) ON DELETE CASCADE,
	user_id   INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	PRIMARY KEY (system_id, user_id)
);

-- Backup and prune configuration of a client system, maintained by a backupper.
CREATE TABLE backup_plans (
	system_id        INTEGER PRIMARY KEY REFERENCES systems (id) ON DELETE CASCADE,
	enabled          INTEGER NOT NULL DEFAULT 0,
	paths            TEXT    NOT NULL DEFAULT '',
	excludes         TEXT    NOT NULL DEFAULT '',
	compression      TEXT    NOT NULL DEFAULT 'lz4',
	one_file_system  INTEGER NOT NULL DEFAULT 0,
	schedule         TEXT    NOT NULL DEFAULT '0 * * * *',
	prune_enabled    INTEGER NOT NULL DEFAULT 0,
	prune_schedule   TEXT    NOT NULL DEFAULT '30 3 * * *',
	keep_hourly      INTEGER NOT NULL DEFAULT 24,
	keep_daily       INTEGER NOT NULL DEFAULT 7,
	keep_weekly      INTEGER NOT NULL DEFAULT 4,
	keep_monthly     INTEGER NOT NULL DEFAULT 6,
	keep_yearly      INTEGER NOT NULL DEFAULT 0,
	next_backup_at   INTEGER NOT NULL DEFAULT 0,
	next_prune_at    INTEGER NOT NULL DEFAULT 0,
	updated_by       TEXT    NOT NULL DEFAULT '',
	updated_at       INTEGER NOT NULL DEFAULT 0
);

-- History of backup, prune, delete and restore runs.
CREATE TABLE jobs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	system_id   INTEGER NOT NULL REFERENCES systems (id) ON DELETE CASCADE,
	kind        TEXT    NOT NULL CHECK (kind IN ('backup', 'prune', 'delete', 'restore')),
	trigger     TEXT    NOT NULL CHECK (trigger IN ('schedule', 'manual')),
	username    TEXT    NOT NULL DEFAULT '',
	status      TEXT    NOT NULL CHECK (status IN ('running', 'success', 'warning', 'failed')),
	archive     TEXT    NOT NULL DEFAULT '',
	started_at  INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	output      TEXT    NOT NULL DEFAULT '',
	-- restores only: where the files go (target_id NULL is the Locutus
	-- container), which archive paths and how
	target_id   INTEGER REFERENCES systems (id) ON DELETE SET NULL,
	target_name TEXT    NOT NULL DEFAULT '',
	destination TEXT    NOT NULL DEFAULT '',
	paths       TEXT    NOT NULL DEFAULT '',
	dry_run     INTEGER NOT NULL DEFAULT 0,
	overwrite   INTEGER NOT NULL DEFAULT 0,
	bytes       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX jobs_system ON jobs (system_id, started_at DESC);

-- History of file restores (downloads).
CREATE TABLE restores (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	system_id  INTEGER NOT NULL REFERENCES systems (id) ON DELETE CASCADE,
	username   TEXT    NOT NULL,
	archive    TEXT    NOT NULL,
	paths      TEXT    NOT NULL,
	status     TEXT    NOT NULL DEFAULT 'success',
	bytes      INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX restores_system ON restores (system_id, created_at DESC);

-- Settings maintained by the admin, such as how long logging is kept.
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`,
	// v1.1: mail the output of jobs. The mail server itself is stored in settings.
	`
ALTER TABLE jobs ADD COLUMN notify_email TEXT NOT NULL DEFAULT '';
ALTER TABLE backup_plans ADD COLUMN backup_email TEXT NOT NULL DEFAULT '';
ALTER TABLE backup_plans ADD COLUMN prune_email TEXT NOT NULL DEFAULT '';
`,
	// v1.2: systems in a Locutus client container. Such a system is stored
	// with location 'remote' and client_container = 1; Locutus pushes the
	// configuration of the container with its management key.
	`
ALTER TABLE systems ADD COLUMN client_container INTEGER NOT NULL DEFAULT 0;
-- clients only: may files be restored to the client
ALTER TABLE systems ADD COLUMN allow_restore INTEGER NOT NULL DEFAULT 1;

CREATE TABLE client_containers (
	host        TEXT    NOT NULL COLLATE NOCASE,
	port        INTEGER NOT NULL,
	-- encrypted private key with which Locutus pushes the configuration
	manager_key TEXT    NOT NULL DEFAULT '',
	pushed_at   INTEGER NOT NULL DEFAULT 0,
	push_ok     INTEGER NOT NULL DEFAULT 0,
	push_output TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (host, port)
);
`,
}

func migrate(database *sql.DB) error {
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := database.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return err
	}
	for i := current; i < len(migrations); i++ {
		tx, err := database.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
