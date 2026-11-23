package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestUpgradeFromV10 opens a database as v1.0 left it (only the first
// migration applied, with data) and checks that it is brought up to date.
func TestUpgradeFromV10(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locutus.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		migrations[0],
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (version) VALUES (1)`,
		`INSERT INTO systems (name, kind, location, created_at) VALUES ('repo', 'repository', 'local', 1)`,
		`INSERT INTO backup_plans (system_id, enabled) VALUES (1, 1)`,
		`INSERT INTO jobs (system_id, kind, trigger, status, started_at) VALUES (1, 'backup', 'manual', 'success', 1)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	old.Close()

	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var version int
	database.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version)
	if version != len(migrations) {
		t.Errorf("schema version %d, want %d", version, len(migrations))
	}
	var jobEmail, backupEmail, pruneEmail string
	if err := database.QueryRow(`SELECT notify_email FROM jobs`).Scan(&jobEmail); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT backup_email, prune_email FROM backup_plans`).Scan(&backupEmail, &pruneEmail); err != nil {
		t.Fatal(err)
	}
	if jobEmail != "" || backupEmail != "" || pruneEmail != "" {
		t.Errorf("unexpected defaults %q %q %q", jobEmail, backupEmail, pruneEmail)
	}

	// Opening again changes nothing.
	database.Close()
	if database, err = Open(path); err != nil {
		t.Fatal(err)
	}
}
