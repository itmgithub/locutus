package models

import (
	"path/filepath"
	"testing"
	"time"

	"itmon.nl/locutus/internal/db"
)

func TestLogging(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := NewStore(database, nil)
	if _, err := database.Exec(`INSERT INTO systems (id, name, kind, location, created_at) VALUES (1, 'web', 'client', 'local', 0)`); err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	old, recent := time.Now().Add(-10*day).Unix(), time.Now().Add(-2*day).Unix()
	for _, j := range []struct {
		kind, status string
		at           int64
	}{
		{"backup", "success", old}, {"backup", "running", old}, {"backup", "success", recent},
		{"restore", "failed", old}, {"prune", "success", old}, {"delete", "success", recent},
	} {
		if _, err := database.Exec(`INSERT INTO jobs (system_id, kind, trigger, status, started_at) VALUES (1, ?, 'manual', ?, ?)`,
			j.kind, j.status, j.at); err != nil {
			t.Fatal(err)
		}
	}
	database.Exec(`INSERT INTO restores (system_id, username, archive, paths, created_at) VALUES (1, 'u', 'a', 'x', ?)`, old)

	count := func() map[string]int {
		stats, err := st.LogStats()
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]int{}
		for _, s := range stats {
			m[s.Category] = s.Count
		}
		return m
	}
	if c := count(); c["backup"] != 3 || c["restore"] != 2 || c["other"] != 2 {
		t.Fatalf("counts = %v", c)
	}

	// Manual: other logging from before 5 days ago.
	n, err := st.PurgeLogs(LogOther, time.Now().Add(-5*day))
	if err != nil || n != 1 {
		t.Fatalf("purge other: n=%d err=%v", n, err)
	}

	// Automatic: keep 5 days of backups, restores forever. The running job stays.
	if err := st.SetLogRetention(map[string]int{LogBackup: 5, LogRestore: 0}); err != nil {
		t.Fatal(err)
	}
	if n, err = st.ApplyLogRetention(time.Now()); err != nil || n != 1 {
		t.Fatalf("retention: n=%d err=%v", n, err)
	}
	if c := count(); c["backup"] != 2 || c["restore"] != 2 || c["other"] != 1 {
		t.Fatalf("counts after cleanup = %v", c)
	}
	if r, _ := st.LogRetention(); r[LogBackup] != 5 || r[LogRestore] != 0 || r[LogOther] != 0 {
		t.Fatalf("retention = %v", r)
	}
}
