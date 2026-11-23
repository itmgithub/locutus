package models

import (
	"database/sql"
	"strings"
	"time"
)

// Default retention: what a new backup plan keeps when pruning.
const (
	DefaultKeepHourly  = 24
	DefaultKeepDaily   = 7
	DefaultKeepWeekly  = 4
	DefaultKeepMonthly = 6
)

// BackupPlan is the backup and prune configuration of a client system.
// Schedules are 5-field cron expressions (minute hour day month weekday).
type BackupPlan struct {
	SystemID      int64
	Enabled       bool
	Paths         []string
	Excludes      []string
	Compression   string
	OneFileSystem bool
	Schedule      string
	PruneEnabled  bool
	PruneSchedule string
	KeepHourly    int
	KeepDaily     int
	KeepWeekly    int
	KeepMonthly   int
	KeepYearly    int
	// Where the output of scheduled backups and prunes is mailed; "" is
	// nowhere. A comma separated list of addresses.
	BackupEmail  string
	PruneEmail   string
	NextBackupAt time.Time
	NextPruneAt  time.Time
	UpdatedBy    string
	UpdatedAt    time.Time
}

// DefaultPlan returns the plan used for a client without configuration.
func DefaultPlan(systemID int64) *BackupPlan {
	return &BackupPlan{
		SystemID:      systemID,
		Compression:   "lz4",
		Schedule:      "0 * * * *",
		PruneSchedule: "30 3 * * *",
		KeepHourly:    DefaultKeepHourly,
		KeepDaily:     DefaultKeepDaily,
		KeepWeekly:    DefaultKeepWeekly,
		KeepMonthly:   DefaultKeepMonthly,
	}
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

const planColumns = `system_id, enabled, paths, excludes, compression, one_file_system, schedule, prune_enabled,
	prune_schedule, keep_hourly, keep_daily, keep_weekly, keep_monthly, keep_yearly, next_backup_at, next_prune_at,
	updated_by, updated_at, backup_email, prune_email`

func scanPlan(row interface{ Scan(...any) error }) (*BackupPlan, error) {
	var p BackupPlan
	var paths, excludes string
	var nextBackup, nextPrune, updated int64
	if err := row.Scan(&p.SystemID, &p.Enabled, &paths, &excludes, &p.Compression, &p.OneFileSystem, &p.Schedule,
		&p.PruneEnabled, &p.PruneSchedule, &p.KeepHourly, &p.KeepDaily, &p.KeepWeekly, &p.KeepMonthly, &p.KeepYearly,
		&nextBackup, &nextPrune, &p.UpdatedBy, &updated, &p.BackupEmail, &p.PruneEmail); err != nil {
		return nil, err
	}
	p.Paths, p.Excludes = splitLines(paths), splitLines(excludes)
	p.NextBackupAt, p.NextPruneAt, p.UpdatedAt = unixTime(nextBackup), unixTime(nextPrune), unixTime(updated)
	return &p, nil
}

// GetPlan returns the plan of a client, or the default plan if none is stored.
func (st *Store) GetPlan(systemID int64) (*BackupPlan, error) {
	p, err := scanPlan(st.db.QueryRow(`SELECT `+planColumns+` FROM backup_plans WHERE system_id = ?`, systemID))
	if err == sql.ErrNoRows {
		return DefaultPlan(systemID), nil
	}
	return p, err
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// SavePlan stores a plan, including the next run times.
func (st *Store) SavePlan(p *BackupPlan) error {
	_, err := st.db.Exec(`INSERT INTO backup_plans (`+planColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (system_id) DO UPDATE SET enabled = excluded.enabled, paths = excluded.paths,
		excludes = excluded.excludes, compression = excluded.compression, one_file_system = excluded.one_file_system,
		schedule = excluded.schedule, prune_enabled = excluded.prune_enabled, prune_schedule = excluded.prune_schedule,
		keep_hourly = excluded.keep_hourly, keep_daily = excluded.keep_daily, keep_weekly = excluded.keep_weekly,
		keep_monthly = excluded.keep_monthly, keep_yearly = excluded.keep_yearly, next_backup_at = excluded.next_backup_at,
		next_prune_at = excluded.next_prune_at, updated_by = excluded.updated_by, updated_at = excluded.updated_at,
		backup_email = excluded.backup_email, prune_email = excluded.prune_email`,
		p.SystemID, p.Enabled, strings.Join(p.Paths, "\n"), strings.Join(p.Excludes, "\n"), p.Compression,
		p.OneFileSystem, p.Schedule, p.PruneEnabled, p.PruneSchedule, p.KeepHourly, p.KeepDaily, p.KeepWeekly,
		p.KeepMonthly, p.KeepYearly, unixOrZero(p.NextBackupAt), unixOrZero(p.NextPruneAt), p.UpdatedBy, now(),
		p.BackupEmail, p.PruneEmail)
	return err
}

// ListActivePlans returns all plans with an enabled backup or prune schedule.
func (st *Store) ListActivePlans() ([]*BackupPlan, error) {
	rows, err := st.db.Query(`SELECT ` + planColumns + ` FROM backup_plans WHERE enabled = 1 OR prune_enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*BackupPlan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

// SetNextBackup records when the next scheduled backup runs.
func (st *Store) SetNextBackup(systemID int64, t time.Time) error {
	_, err := st.db.Exec(`UPDATE backup_plans SET next_backup_at = ? WHERE system_id = ?`, unixOrZero(t), systemID)
	return err
}

// SetNextPrune records when the next scheduled prune runs.
func (st *Store) SetNextPrune(systemID int64, t time.Time) error {
	_, err := st.db.Exec(`UPDATE backup_plans SET next_prune_at = ? WHERE system_id = ?`, unixOrZero(t), systemID)
	return err
}
