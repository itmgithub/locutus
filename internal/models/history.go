package models

import (
	"strings"
	"time"
)

// Job kinds, triggers and statuses.
const (
	JobBackup  = "backup"
	JobPrune   = "prune"
	JobDelete  = "delete"  // deletes one archive (Job.Archive)
	JobRestore = "restore" // restores Job.Paths of Job.Archive to a target

	TriggerSchedule = "schedule"
	TriggerManual   = "manual"

	StatusRunning = "running"
	StatusSuccess = "success"
	StatusWarning = "warning"
	StatusFailed  = "failed"
)

// maxJobOutput limits the stored borg output per job.
const maxJobOutput = 64 * 1024

// Job is one backup, prune, delete or restore run of a system.
type Job struct {
	ID         int64
	SystemID   int64
	SystemName string
	Kind       string
	Trigger    string
	Username   string
	Status     string
	Archive    string
	StartedAt  time.Time
	FinishedAt time.Time
	Output     string

	// Restores only. TargetID 0 is the Locutus container itself.
	TargetID    int64
	TargetName  string
	Destination string
	Paths       []string
	DryRun      bool
	Overwrite   bool
	Bytes       int64
}

// LocalTarget reports whether a restore writes into the Locutus container.
func (j *Job) LocalTarget() bool { return j.TargetID == 0 && j.TargetName == "" }

// Duration returns how long the job ran (so far).
func (j *Job) Duration() time.Duration {
	end := j.FinishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(j.StartedAt).Round(time.Second)
}

const jobColumns = `j.id, j.system_id, s.name, j.kind, j.trigger, j.username, j.status, j.archive, j.started_at, j.finished_at,
	COALESCE(j.target_id, 0), j.target_name, j.destination, j.paths, j.dry_run, j.overwrite, j.bytes`

func scanJob(row interface{ Scan(...any) error }, withOutput bool) (*Job, error) {
	var j Job
	var started, finished int64
	var paths string
	dest := []any{&j.ID, &j.SystemID, &j.SystemName, &j.Kind, &j.Trigger, &j.Username, &j.Status, &j.Archive, &started, &finished,
		&j.TargetID, &j.TargetName, &j.Destination, &paths, &j.DryRun, &j.Overwrite, &j.Bytes}
	if withOutput {
		dest = append(dest, &j.Output)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, notFound(err)
	}
	j.StartedAt, j.FinishedAt, j.Paths = unixTime(started), unixTime(finished), splitLines(paths)
	return &j, nil
}

// CreateJob records the start of a job.
func (st *Store) CreateJob(j *Job) error {
	j.StartedAt, j.Status = time.Now(), StatusRunning
	res, err := st.db.Exec(`INSERT INTO jobs (system_id, kind, trigger, username, status, archive, started_at,
		target_id, target_name, destination, paths, dry_run, overwrite)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, j.SystemID, j.Kind, j.Trigger, j.Username, j.Status, j.Archive,
		j.StartedAt.Unix(), nullID(j.TargetID), j.TargetName, j.Destination, strings.Join(j.Paths, "\n"), j.DryRun, j.Overwrite)
	if err != nil {
		return err
	}
	j.ID, err = res.LastInsertId()
	return err
}

// FinishJob records the result of a job; bytes is the amount of data
// restored (restores only).
func (st *Store) FinishJob(id int64, status, output string, bytes int64) error {
	if len(output) > maxJobOutput {
		output = "…\n" + output[len(output)-maxJobOutput:]
	}
	_, err := st.db.Exec(`UPDATE jobs SET status = ?, output = ?, bytes = ?, finished_at = ? WHERE id = ?`,
		status, output, bytes, now(), id)
	return err
}

// FailRunningJobs marks jobs that were running when Locutus stopped as failed.
func (st *Store) FailRunningJobs(note string) error {
	_, err := st.db.Exec(`UPDATE jobs SET status = 'failed', output = output || ?, finished_at = ? WHERE status = 'running'`,
		"\n"+note, now())
	return err
}

// GetJob returns one job including its output.
func (st *Store) GetJob(id int64) (*Job, error) {
	return scanJob(st.db.QueryRow(`SELECT `+jobColumns+`, j.output FROM jobs j JOIN systems s ON s.id = j.system_id
		WHERE j.id = ?`, id), true)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func idArgs(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// ListJobs returns the latest backup, prune and delete jobs of the given
// systems. Restore jobs are listed by ListRestores.
func (st *Store) ListJobs(systemIDs []int64, limit int) ([]*Job, error) {
	if len(systemIDs) == 0 {
		return nil, nil
	}
	rows, err := st.db.Query(`SELECT `+jobColumns+` FROM jobs j JOIN systems s ON s.id = j.system_id
		WHERE j.kind != 'restore' AND j.system_id IN (`+placeholders(len(systemIDs))+`)
		ORDER BY j.started_at DESC, j.id DESC LIMIT ?`,
		append(idArgs(systemIDs), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*Job
	for rows.Next() {
		j, err := scanJob(rows, false)
		if err != nil {
			return nil, err
		}
		list = append(list, j)
	}
	return list, rows.Err()
}

// LastJob returns the most recent job of a kind for a system, or nil.
func (st *Store) LastJob(systemID int64, kind string) (*Job, error) {
	j, err := scanJob(st.db.QueryRow(`SELECT `+jobColumns+` FROM jobs j JOIN systems s ON s.id = j.system_id
		WHERE j.system_id = ? AND j.kind = ? ORDER BY j.started_at DESC, j.id DESC LIMIT 1`, systemID, kind), false)
	if err == ErrNotFound {
		return nil, nil
	}
	return j, err
}

// Restore is one download of files from an archive, or (JobID set) one
// restore job that writes them to a client or into the Locutus container.
type Restore struct {
	ID         int64
	SystemID   int64
	SystemName string
	Username   string
	Archive    string
	Paths      []string
	Status     string
	Bytes      int64
	CreatedAt  time.Time

	JobID       int64 // 0 for a download
	TargetID    int64
	TargetName  string
	Destination string
	DryRun      bool
	Overwrite   bool
}

// LocalTarget reports whether a restore job writes into the Locutus container.
func (r *Restore) LocalTarget() bool { return r.TargetID == 0 && r.TargetName == "" }

// RecordRestore stores a restore in the history.
func (st *Store) RecordRestore(r *Restore) error {
	res, err := st.db.Exec(`INSERT INTO restores (system_id, username, archive, paths, status, bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, r.SystemID, r.Username, r.Archive, strings.Join(r.Paths, "\n"), r.Status, r.Bytes, now())
	if err != nil {
		return err
	}
	r.ID, err = res.LastInsertId()
	return err
}

// ListRestores returns the latest downloads and restore jobs of the given systems.
func (st *Store) ListRestores(systemIDs []int64, limit int) ([]*Restore, error) {
	if len(systemIDs) == 0 {
		return nil, nil
	}
	in := placeholders(len(systemIDs))
	args := append(idArgs(systemIDs), idArgs(systemIDs)...)
	rows, err := st.db.Query(`SELECT * FROM (
		SELECT r.id, 0 AS job_id, r.system_id, s.name, r.username, r.archive, r.paths, r.status, r.bytes, r.created_at,
			0, '', '', 0, 0
		FROM restores r JOIN systems s ON s.id = r.system_id WHERE r.system_id IN (`+in+`)
		UNION ALL
		SELECT 0, j.id, j.system_id, s.name, j.username, j.archive, j.paths, j.status, j.bytes, j.started_at,
			COALESCE(j.target_id, 0), j.target_name, j.destination, j.dry_run, j.overwrite
		FROM jobs j JOIN systems s ON s.id = j.system_id WHERE j.kind = 'restore' AND j.system_id IN (`+in+`)
	) ORDER BY 10 DESC, 2 DESC, 1 DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*Restore
	for rows.Next() {
		var r Restore
		var paths string
		var created int64
		if err := rows.Scan(&r.ID, &r.JobID, &r.SystemID, &r.SystemName, &r.Username, &r.Archive, &paths, &r.Status, &r.Bytes,
			&created, &r.TargetID, &r.TargetName, &r.Destination, &r.DryRun, &r.Overwrite); err != nil {
			return nil, err
		}
		r.Paths, r.CreatedAt = splitLines(paths), unixTime(created)
		list = append(list, &r)
	}
	return list, rows.Err()
}
