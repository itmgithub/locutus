// Package scheduler is the background process that starts scheduled backups
// and prunes of client systems, and runs manually triggered jobs (backups,
// prunes, deletes and restores).
package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"itmon.nl/locutus/internal/borg"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/schedule"
)

// tick is how often the plans are checked.
const tick = 20 * time.Second

// jobTimeout bounds a single backup or prune run.
const jobTimeout = 24 * time.Hour

// ErrBusy is returned when a job of the client is already running.
var ErrBusy = errors.New("a job for this system is already running")

// Scheduler runs backup and prune jobs, at most one at a time per client.
type Scheduler struct {
	store *models.Store
	borg  *borg.Service

	mu      sync.Mutex
	running map[int64]bool // client system id -> job running
	wg      sync.WaitGroup
	ctx     context.Context
}

// New creates a scheduler.
func New(store *models.Store, b *borg.Service) *Scheduler {
	return &Scheduler{store: store, borg: b, running: map[int64]bool{}, ctx: context.Background()}
}

// Run checks the plans until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	if err := s.store.FailRunningJobs("Locutus was restarted while this job was running."); err != nil {
		log.Printf("scheduler: %v", err)
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		s.checkPlans()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Wait waits (at most timeout) for running jobs after Run was stopped.
func (s *Scheduler) Wait(timeout time.Duration) {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// IsRunning reports whether a job of the client is running.
func (s *Scheduler) IsRunning(systemID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[systemID]
}

func (s *Scheduler) checkPlans() {
	plans, err := s.store.ListActivePlans()
	if err != nil {
		log.Printf("scheduler: list plans: %v", err)
		return
	}
	now := time.Now()
	for _, p := range plans {
		// Plans without a next run time get one (e.g. after a restart with a
		// schedule that could not be computed before).
		if p.Enabled {
			if p.NextBackupAt.IsZero() {
				s.store.SetNextBackup(p.SystemID, schedule.NextRun(p.Schedule, now))
			} else if !p.NextBackupAt.After(now) {
				s.store.SetNextBackup(p.SystemID, schedule.NextRun(p.Schedule, now))
				if _, err := s.Start(p.SystemID, models.JobBackup, models.TriggerSchedule, ""); err != nil {
					log.Printf("scheduler: backup of system %d: %v", p.SystemID, err)
				}
				continue // the prune waits for the next tick
			}
		}
		if p.PruneEnabled {
			if p.NextPruneAt.IsZero() {
				s.store.SetNextPrune(p.SystemID, schedule.NextRun(p.PruneSchedule, now))
			} else if !p.NextPruneAt.After(now) && !s.IsRunning(p.SystemID) {
				s.store.SetNextPrune(p.SystemID, schedule.NextRun(p.PruneSchedule, now))
				if _, err := s.Start(p.SystemID, models.JobPrune, models.TriggerSchedule, ""); err != nil {
					log.Printf("scheduler: prune of system %d: %v", p.SystemID, err)
				}
			}
		}
	}
}

// Start begins a backup or prune of a client in the background and returns
// the job record.
func (s *Scheduler) Start(systemID int64, kind, trigger, username string) (*models.Job, error) {
	if kind != models.JobBackup && kind != models.JobPrune {
		return nil, fmt.Errorf("unknown job kind %q", kind)
	}
	return s.start(&models.Job{SystemID: systemID, Kind: kind, Trigger: trigger, Username: username})
}

// StartDelete begins deleting one archive of a client in the background.
func (s *Scheduler) StartDelete(systemID int64, archive, username string) (*models.Job, error) {
	return s.start(&models.Job{SystemID: systemID, Kind: models.JobDelete, Trigger: models.TriggerManual,
		Username: username, Archive: archive})
}

// RestoreRequest describes a restore started by a user.
type RestoreRequest struct {
	SystemID    int64          // the client or repository whose archive is restored
	Archive     string         //
	Paths       []string       // archive paths, see borg.RestoreOptions
	Target      *models.System // client to restore to; nil is the Locutus container
	Destination string
	DryRun      bool
	Overwrite   bool
	Username    string
}

// StartRestore begins a restore in the background. It runs as a job of the
// system the archive belongs to.
func (s *Scheduler) StartRestore(req RestoreRequest) (*models.Job, error) {
	if !borg.ValidDestination(req.Destination) {
		return nil, fmt.Errorf("invalid destination %q", req.Destination)
	}
	if len(req.Paths) == 0 {
		return nil, errors.New("nothing to restore")
	}
	job := &models.Job{SystemID: req.SystemID, Kind: models.JobRestore, Trigger: models.TriggerManual, Username: req.Username,
		Archive: req.Archive, Paths: req.Paths, Destination: req.Destination, DryRun: req.DryRun, Overwrite: req.Overwrite}
	if req.Target != nil {
		if !req.Target.IsClient() {
			return nil, errors.New("a restore can only go to a client")
		}
		job.TargetID, job.TargetName = req.Target.ID, req.Target.Name
	}
	return s.start(job)
}

func (s *Scheduler) start(job *models.Job) (*models.Job, error) {
	systemID := job.SystemID
	s.mu.Lock()
	if s.running[systemID] {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.running[systemID] = true
	ctx := s.ctx
	s.mu.Unlock()

	release := func() {
		s.mu.Lock()
		delete(s.running, systemID)
		s.mu.Unlock()
	}
	sys, err := s.store.GetSystem(systemID)
	switch {
	case err != nil:
	case job.Kind == models.JobRestore:
		if repositoryOf(sys) == nil {
			err = errors.New("system has no repository")
		} else if sys.IsClient() && !strings.HasPrefix(job.Archive, sys.ArchivePrefix+".") {
			err = errors.New("the archive does not belong to this client")
		}
	case !sys.IsClient() || sys.Repository == nil:
		err = errors.New("system is not a client with a repository")
	case job.Kind == models.JobDelete && !strings.HasPrefix(job.Archive, sys.ArchivePrefix+"."):
		err = errors.New("the archive does not belong to this client")
	case job.Kind == models.JobBackup:
		job.Archive = borg.ArchiveName(sys.ArchivePrefix, time.Now())
	}
	if err == nil {
		err = s.store.CreateJob(job)
	}
	if err != nil {
		release()
		return nil, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer release()
		s.execute(ctx, sys, job)
	}()
	return job, nil
}

// repositoryOf returns the repository holding the archives of a system.
func repositoryOf(sys *models.System) *models.System {
	if sys.IsRepository() {
		return sys
	}
	return sys.Repository
}

// execute runs the job and stores the result.
func (s *Scheduler) execute(parent context.Context, sys *models.System, job *models.Job) {
	ctx, cancel := context.WithTimeout(parent, jobTimeout)
	defer cancel()
	var out bytes.Buffer
	rc, err := s.runJob(ctx, sys, job, &out)
	status := models.StatusSuccess
	switch {
	case err != nil:
		status = models.StatusFailed
		fmt.Fprintf(&out, "\nError: %v\n", err)
	case rc == borg.RCWarning:
		status = models.StatusWarning
	case rc != borg.RCSuccess:
		status = models.StatusFailed
		fmt.Fprintf(&out, "\nborg exit code %d\n", rc)
	}
	if err := s.store.FinishJob(job.ID, status, out.String(), job.Bytes); err != nil {
		log.Printf("scheduler: store job %d: %v", job.ID, err)
	}
	log.Printf("%s of %s finished: %s", job.Kind, sys.Name, status)
}

func (s *Scheduler) runJob(ctx context.Context, sys *models.System, job *models.Job, out *bytes.Buffer) (int, error) {
	repo, err := RepoFor(s.store, repositoryOf(sys))
	if err != nil {
		return -1, err
	}
	if job.Kind == models.JobRestore {
		return s.runRestore(ctx, repo, job, out)
	}
	plan, err := s.store.GetPlan(sys.ID)
	if err != nil {
		return -1, err
	}
	switch job.Kind {
	case models.JobBackup:
		if len(plan.Paths) == 0 {
			return -1, errors.New("no paths configured for the backup")
		}
		client := borg.Client{Local: !sys.IsRemote(), Address: sys.SSHAddress(), User: sys.SSHUser, Sudo: sys.UseSudo}
		if sys.IsRemote() {
			sec, err := s.store.SystemSecrets(sys.ID)
			if err != nil {
				return -1, err
			}
			if sec.SSHKey == "" {
				return -1, errors.New("no SSH key configured for the client")
			}
			client.SSHKey = sec.SSHKey
		}
		opts := borg.CreateOptions{Paths: plan.Paths, Excludes: plan.Excludes, Compression: plan.Compression,
			OneFileSystem: plan.OneFileSystem}
		return s.borg.Create(ctx, client, repo, job.Archive, opts, out)
	case models.JobPrune:
		keep := borg.Retention{Hourly: plan.KeepHourly, Daily: plan.KeepDaily, Weekly: plan.KeepWeekly,
			Monthly: plan.KeepMonthly, Yearly: plan.KeepYearly}
		return s.borg.Prune(ctx, repo, sys.ArchiveGlob(), keep, out)
	case models.JobDelete:
		fmt.Fprintf(out, "Deleting archive %s\n", job.Archive)
		return s.borg.Delete(ctx, repo, job.Archive, out)
	}
	return -1, fmt.Errorf("unknown job kind %q", job.Kind)
}

// runRestore restores the paths of a restore job to its target.
func (s *Scheduler) runRestore(ctx context.Context, repo *borg.Repo, job *models.Job, out *bytes.Buffer) (int, error) {
	target := s.borg.LocalTarget()
	where := "the Locutus container"
	if job.TargetID != 0 {
		sys, err := s.store.GetSystem(job.TargetID)
		if err != nil {
			return -1, fmt.Errorf("restore target: %w", err)
		}
		if !sys.IsClient() {
			return -1, errors.New("a restore can only go to a client")
		}
		if sys.IsRemote() {
			sec, err := s.store.SystemSecrets(sys.ID)
			if err != nil {
				return -1, err
			}
			if sec.SSHKey == "" {
				return -1, errors.New("no SSH key configured for the client")
			}
			target = s.borg.RemoteTarget(borg.Client{Address: sys.SSHAddress(), User: sys.SSHUser, SSHKey: sec.SSHKey, Sudo: sys.UseSudo})
			where = fmt.Sprintf("client %s (%s@%s)", sys.Name, sys.SSHUser, sys.SSHAddress())
			if sys.UseSudo {
				where += " with sudo"
			}
		} else {
			where = fmt.Sprintf("client %s (the Locutus container)", sys.Name)
		}
	} else if job.TargetName != "" {
		return -1, fmt.Errorf("the restore target %s no longer exists", job.TargetName)
	}
	fmt.Fprintf(out, "Restoring from archive %s:\n", job.Archive)
	for _, p := range job.Paths {
		fmt.Fprintf(out, "  /%s\n", p)
	}
	fmt.Fprintf(out, "Target: %s\nDestination: %s\nOverwrite existing files: %s\n", where, job.Destination,
		map[bool]string{true: "yes", false: "no"}[job.Overwrite])
	tree, err := s.borg.Tree(ctx, repo, job.Archive)
	if err != nil {
		return -1, err
	}
	rc, n, err := s.borg.Restore(ctx, repo, tree, borg.RestoreOptions{Archive: job.Archive, Paths: job.Paths,
		Destination: job.Destination, DryRun: job.DryRun, Overwrite: job.Overwrite}, target, out)
	job.Bytes = n
	return rc, err
}

// RepoFor builds the borg repository description, including the secrets,
// of a repository system.
func RepoFor(store *models.Store, repoSys *models.System) (*borg.Repo, error) {
	if repoSys == nil || !repoSys.IsRepository() {
		return nil, errors.New("no repository configured")
	}
	sec, err := store.SystemSecrets(repoSys.ID)
	if err != nil {
		return nil, err
	}
	return &borg.Repo{
		Location:    repoSys.RepoLocation(),
		Remote:      repoSys.IsRemote(),
		SSHKey:      sec.SSHKey,
		PruneSSHKey: sec.PruneSSHKey,
		Passphrase:  sec.Passphrase,
		Version:     repoSys.BorgVersion,
	}, nil
}
