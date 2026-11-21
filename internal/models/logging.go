package models

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// Logging categories. The logging is the history of jobs (with their output)
// and downloads; it can be cleaned up per category.
const (
	LogBackup  = "backup"  // backup jobs
	LogRestore = "restore" // restore jobs and downloads
	LogOther   = "other"   // prune and delete jobs
)

// LogCategories lists the categories in display order.
var LogCategories = []string{LogBackup, LogRestore, LogOther}

// ValidLogCategory reports whether c is a logging category.
func ValidLogCategory(c string) bool {
	for _, x := range LogCategories {
		if x == c {
			return true
		}
	}
	return false
}

// jobKinds returns the SQL condition on jobs.kind for a category.
func jobKinds(category string) string {
	switch category {
	case LogBackup:
		return `kind = 'backup'`
	case LogRestore:
		return `kind = 'restore'`
	default:
		return `kind IN ('prune', 'delete')`
	}
}

// LogStats describes the logging of one category.
type LogStats struct {
	Category      string
	Count         int
	Oldest        time.Time
	RetentionDays int // 0: kept forever
}

// LogStats returns the number of entries, the oldest entry and the retention
// of every category.
func (st *Store) LogStats() ([]LogStats, error) {
	retention, err := st.LogRetention()
	if err != nil {
		return nil, err
	}
	var list []LogStats
	for _, c := range LogCategories {
		s := LogStats{Category: c, RetentionDays: retention[c]}
		var oldest sql.NullInt64
		if err := st.db.QueryRow(`SELECT COUNT(*), MIN(started_at) FROM jobs WHERE `+jobKinds(c)).Scan(&s.Count, &oldest); err != nil {
			return nil, err
		}
		if c == LogRestore {
			var n int
			var oldestDownload sql.NullInt64
			if err := st.db.QueryRow(`SELECT COUNT(*), MIN(created_at) FROM restores`).Scan(&n, &oldestDownload); err != nil {
				return nil, err
			}
			s.Count += n
			if oldestDownload.Valid && (!oldest.Valid || oldestDownload.Int64 < oldest.Int64) {
				oldest = oldestDownload
			}
		}
		if oldest.Valid {
			s.Oldest = unixTime(oldest.Int64)
		}
		list = append(list, s)
	}
	return list, nil
}

// PurgeLogs deletes the logging of a category from before the given time.
// Running jobs are never deleted. It returns the number of deleted entries.
func (st *Store) PurgeLogs(category string, before time.Time) (int64, error) {
	if !ValidLogCategory(category) {
		return 0, fmt.Errorf("unknown logging category %q", category)
	}
	res, err := st.db.Exec(`DELETE FROM jobs WHERE `+jobKinds(category)+` AND status != 'running' AND started_at < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if category == LogRestore {
		res, err := st.db.Exec(`DELETE FROM restores WHERE created_at < ?`, before.Unix())
		if err != nil {
			return n, err
		}
		m, _ := res.RowsAffected()
		n += m
	}
	return n, nil
}

func retentionKey(category string) string { return "log_retention_days." + category }

// LogRetention returns per category how many days of logging are kept
// (0: forever).
func (st *Store) LogRetention() (map[string]int, error) {
	m := map[string]int{}
	for _, c := range LogCategories {
		v, err := st.Setting(retentionKey(c))
		if err != nil {
			return nil, err
		}
		m[c], _ = strconv.Atoi(v)
	}
	return m, nil
}

// SetLogRetention stores the retention (in days) of the given categories.
func (st *Store) SetLogRetention(days map[string]int) error {
	for c, d := range days {
		if !ValidLogCategory(c) {
			return fmt.Errorf("unknown logging category %q", c)
		}
		if err := st.SetSetting(retentionKey(c), strconv.Itoa(d)); err != nil {
			return err
		}
	}
	return nil
}

// ApplyLogRetention deletes the logging that is older than the retention of
// its category and returns the number of deleted entries.
func (st *Store) ApplyLogRetention(now time.Time) (int64, error) {
	retention, err := st.LogRetention()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, c := range LogCategories {
		if d := retention[c]; d > 0 {
			n, err := st.PurgeLogs(c, now.AddDate(0, 0, -d))
			total += n
			if err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// Setting returns a stored setting, or "" when it is not set.
func (st *Store) Setting(key string) (string, error) {
	var v string
	err := st.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting stores a setting.
func (st *Store) SetSetting(key, value string) error {
	_, err := st.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}
