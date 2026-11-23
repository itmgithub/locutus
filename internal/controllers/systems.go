package controllers

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"itmon.nl/locutus/internal/borg"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/scheduler"
	"itmon.nl/locutus/internal/views"
)

// borgTimeout bounds interactive borg calls (listing archives and items).
const borgTimeout = 10 * time.Minute

// userSystem loads the system from the path, but only when it is assigned
// to the current user.
func (a *App) userSystem(w http.ResponseWriter, r *http.Request) *models.System {
	id := pathID(r, "id")
	ok, err := a.store.UserHasSystem(currentUser(r).ID, id)
	if err != nil {
		a.serverError(w, r, err)
		return nil
	}
	if !ok {
		a.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return nil
	}
	s, err := a.store.GetSystem(id)
	if err != nil {
		a.storeError(w, r, err)
		return nil
	}
	return s
}

// repositoryOf returns the repository system holding the backups of s.
func repositoryOf(s *models.System) *models.System {
	if s.IsRepository() {
		return s
	}
	return s.Repository
}

// historyIDs returns the systems whose jobs and restores are shown for s:
// the client itself, or a repository together with all of its clients.
func (a *App) historyIDs(s *models.System) ([]int64, error) {
	ids := []int64{s.ID}
	if s.IsRepository() {
		clients, err := a.store.ListRepositoryClients(s.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range clients {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

type systemRow struct {
	System     *models.System
	LastBackup *models.Job
	Plan       *models.BackupPlan
}

func (a *App) systems(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListUserSystems(currentUser(r).ID)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	rows := make([]systemRow, 0, len(list))
	for _, s := range list {
		row := systemRow{System: s}
		if s.IsClient() {
			if row.LastBackup, err = a.store.LastJob(s.ID, models.JobBackup); err != nil {
				a.serverError(w, r, err)
				return
			}
			if row.Plan, err = a.store.GetPlan(s.ID); err != nil {
				a.serverError(w, r, err)
				return
			}
		}
		rows = append(rows, row)
	}
	a.render(w, r, http.StatusOK, "systems", &views.Page{Title: a.t(r, "systems.title"), Data: rows})
}

type overviewData struct {
	System     *models.System
	Repository *models.System
	Archives   []borg.Archive
	ArchiveErr string
	Jobs       []*models.Job
	Restores   []*models.Restore
	Plan       *models.BackupPlan
	BackupDesc string
	PruneDesc  string
	Running    bool
	Retention  map[string]int // days of logging kept per category, 0 = forever
	Notify     notifyField    // "mail the output to" of Back up now and Prune now
}

// systemOverview shows the archives, backup jobs and restores of a system.
func (a *App) systemOverview(w http.ResponseWriter, r *http.Request) {
	s := a.userSystem(w, r)
	if s == nil {
		return
	}
	data := overviewData{System: s, Repository: repositoryOf(s), Running: a.sched.IsRunning(s.ID), Notify: a.notifyField(r)}
	ids, err := a.historyIDs(s)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if data.Jobs, err = a.store.ListJobs(ids, 25); err != nil {
		a.serverError(w, r, err)
		return
	}
	if data.Restores, err = a.store.ListRestores(ids, 25); err != nil {
		a.serverError(w, r, err)
		return
	}
	if data.Retention, err = a.store.LogRetention(); err != nil {
		a.serverError(w, r, err)
		return
	}
	if s.IsClient() {
		if data.Plan, err = a.store.GetPlan(s.ID); err != nil {
			a.serverError(w, r, err)
			return
		}
		data.BackupDesc = a.describeExpr(r, data.Plan.Schedule)
		data.PruneDesc = a.describeExpr(r, data.Plan.PruneSchedule)
	}
	if repo, err := scheduler.RepoFor(a.store, data.Repository); err != nil {
		data.ArchiveErr = err.Error()
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), borgTimeout)
		defer cancel()
		if data.Archives, err = a.borg.Archives(ctx, repo, s.ArchiveGlob()); err != nil {
			log.Printf("list archives of %s: %v", s.Name, err)
			data.ArchiveErr = err.Error()
		}
	}
	a.render(w, r, http.StatusOK, "system", &views.Page{Title: s.Name, Data: data})
}

// archiveFromPath validates the archive name in the path; a client may only
// see its own archives.
func (a *App) archiveFromPath(w http.ResponseWriter, r *http.Request, s *models.System) (string, bool) {
	name := r.PathValue("archive")
	valid := name != "" && len(name) < 256 && !strings.Contains(name, "::") && !strings.ContainsAny(name, "/\n") &&
		!strings.HasPrefix(name, "-")
	if valid && s.IsClient() {
		valid = strings.HasPrefix(name, s.ArchivePrefix+".")
	}
	if !valid {
		a.errorPage(w, r, http.StatusNotFound, "error.not_found")
	}
	return name, valid
}

type browseData struct {
	System  *models.System
	Archive string
	// Restore targets: the clients assigned to the user. DefaultTarget is the
	// client the archive belongs to, when known and assigned; 0 is the
	// Locutus container.
	Targets       []*models.System
	DefaultTarget int64
	Notify        notifyField
}

func (a *App) archiveBrowse(w http.ResponseWriter, r *http.Request) {
	s := a.userSystem(w, r)
	if s == nil {
		return
	}
	archive, ok := a.archiveFromPath(w, r, s)
	if !ok {
		return
	}
	data := browseData{System: s, Archive: archive, Notify: a.notifyField(r)}
	if currentUser(r).CanRestore() {
		targets, err := a.restoreTargets(r)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		data.Targets = targets
		if c := archiveClient(s, archive, targets); c != nil {
			data.DefaultTarget = c.ID
		}
	}
	a.render(w, r, http.StatusOK, "archive", &views.Page{Title: archive, Data: data})
}

// restoreTargets returns the clients the user may restore to.
func (a *App) restoreTargets(r *http.Request) ([]*models.System, error) {
	list, err := a.store.ListUserSystems(currentUser(r).ID)
	if err != nil {
		return nil, err
	}
	var clients []*models.System
	for _, c := range list {
		if c.IsClient() && c.AllowRestore {
			clients = append(clients, c)
		}
	}
	return clients, nil
}

// archiveClient returns the client (among candidates) that made the archive.
func archiveClient(s *models.System, archive string, candidates []*models.System) *models.System {
	prefix, _, _ := strings.Cut(archive, ".")
	repoID := s.ID
	if s.IsClient() {
		repoID = s.RepositoryID
	}
	for _, c := range candidates {
		if (s.IsClient() && c.ID == s.ID) || (s.IsRepository() && c.RepositoryID == repoID && strings.EqualFold(c.ArchivePrefix, prefix)) {
			return c
		}
	}
	return nil
}

func (a *App) archiveTreeFor(r *http.Request, s *models.System, archive string) (*borg.Tree, error) {
	repo, err := scheduler.RepoFor(a.store, repositoryOf(s))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), borgTimeout)
	defer cancel()
	return a.borg.Tree(ctx, repo, archive)
}

// archiveTree returns the entries of one directory of an archive as JSON;
// the browser loads the tree one directory at a time.
func (a *App) archiveTree(w http.ResponseWriter, r *http.Request) {
	s := a.userSystem(w, r)
	if s == nil {
		return
	}
	archive, ok := a.archiveFromPath(w, r, s)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	tree, err := a.archiveTreeFor(r, s, archive)
	if err != nil {
		log.Printf("list items of %s: %v", archive, err)
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": a.t(r, "archive.load_failed")})
		return
	}
	items := tree.Children(r.URL.Query().Get("path"))
	json.NewEncoder(w).Encode(map[string]any{"items": items})
}

// selectedPaths validates the selected paths against the archive tree and
// drops paths that are inside another selected directory.
func selectedPaths(tree *borg.Tree, values []string) []borg.Item {
	seen := map[string]borg.Item{}
	for _, v := range values {
		if it, ok := tree.Lookup(v); ok {
			seen[it.Path] = it
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []borg.Item
	for _, p := range paths {
		if n := len(out); n > 0 && out[n-1].IsDir() && strings.HasPrefix(p, out[n-1].Path+"/") {
			continue
		}
		out = append(out, seen[p])
	}
	return out
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// archiveDownload restores the selected files: a single file is sent as is,
// anything else as a .tar.gz archive.
func (a *App) archiveDownload(w http.ResponseWriter, r *http.Request) {
	s := a.userSystem(w, r)
	if s == nil {
		return
	}
	archive, ok := a.archiveFromPath(w, r, s)
	if !ok {
		return
	}
	tree, err := a.archiveTreeFor(r, s, archive)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	items := selectedPaths(tree, r.Form["path"])
	if len(items) == 0 {
		a.flash(w, r, "error", "archive.nothing_selected")
		http.Redirect(w, r, "/systems/"+itoa(s.ID)+"/archives/"+archive, http.StatusSeeOther)
		return
	}
	repo, err := scheduler.RepoFor(a.store, repositoryOf(s))
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	rec := &models.Restore{SystemID: s.ID, Username: currentUser(r).Username, Archive: archive, Status: models.StatusSuccess}
	paths := make([]string, len(items))
	for i, it := range items {
		paths[i] = it.Path
	}
	rec.Paths = paths

	ctx := r.Context()
	cw := &countingWriter{w: w}
	single := len(items) == 1 && items[0].Type == "-"
	if single {
		name := path.Base(items[0].Path)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		err = a.borg.ExtractFile(ctx, repo, archive, items[0].Path, cw)
	} else {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": archive + ".tar.gz"}))
		gz := gzip.NewWriter(cw)
		raw := &countingWriter{w: gz}
		err = a.borg.ExportTar(ctx, repo, archive, paths, raw)
		// Without any tar data the error can still be reported as a page.
		if err == nil || raw.n > 0 {
			if cerr := gz.Close(); err == nil {
				err = cerr
			}
		}
	}
	rec.Bytes = cw.n
	if err != nil {
		rec.Status = models.StatusFailed
		log.Printf("restore from %s by %s failed: %v", archive, rec.Username, err)
		if cw.n == 0 && !errors.Is(err, context.Canceled) {
			w.Header().Del("Content-Disposition")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, a.t(r, "archive.restore_failed"))
		}
	}
	if err := a.store.RecordRestore(rec); err != nil {
		log.Printf("record restore: %v", err)
	}
}

// archiveRestore starts a restore job: the selected files are written to a
// client (or into the Locutus container) below a destination directory.
func (a *App) archiveRestore(w http.ResponseWriter, r *http.Request) {
	s := a.userSystem(w, r)
	if s == nil {
		return
	}
	archive, ok := a.archiveFromPath(w, r, s)
	if !ok {
		return
	}
	back := "/systems/" + itoa(s.ID) + "/archives/" + archive
	tree, err := a.archiveTreeFor(r, s, archive)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	items := selectedPaths(tree, r.Form["path"])
	if len(items) == 0 {
		a.flash(w, r, "error", "archive.nothing_selected")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	email, ok := a.notifyEmail(r)
	if !ok {
		a.flash(w, r, "error", "error.email_invalid")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	req := scheduler.RestoreRequest{SystemID: s.ID, Archive: archive, Username: currentUser(r).Username,
		DryRun: r.FormValue("dry_run") == "1", Overwrite: r.FormValue("overwrite") == "1",
		NotifyEmail: email, Lang: a.tr.Language(r)}
	for _, it := range items {
		req.Paths = append(req.Paths, it.Path)
	}
	if id := formID(r, "target"); id != 0 {
		targets, err := a.restoreTargets(r)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		for _, c := range targets {
			if c.ID == id {
				req.Target = c
			}
		}
		if req.Target == nil {
			a.errorPage(w, r, http.StatusForbidden, "error.forbidden")
			return
		}
	}
	dest := formText(r, "destination")
	if dest != "" {
		dest = path.Clean(dest)
	}
	if !borg.ValidDestination(dest) {
		a.flash(w, r, "error", "restore.destination_invalid")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	// A destination inside a protected directory of the container is refused
	// right away; with "/" only the protected entries are left out.
	if (req.Target == nil || !req.Target.IsRemote()) && a.borg.LocalTarget().Refused(path.Join(dest, "x")) != "" {
		a.flash(w, r, "error", "restore.destination_protected", dest)
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	req.Destination = dest
	job, err := a.sched.StartRestore(req)
	if errors.Is(err, scheduler.ErrBusy) {
		a.flash(w, r, "error", "error.job_running")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/systems/"+itoa(s.ID)+"/jobs/"+itoa(job.ID), http.StatusSeeOther)
}

// jobDetail shows the output of a backup, prune, delete or restore job.
func (a *App) jobDetail(w http.ResponseWriter, r *http.Request) {
	s := a.userSystem(w, r)
	if s == nil {
		return
	}
	job, err := a.store.GetJob(pathID(r, "jid"))
	if err != nil {
		a.storeError(w, r, err)
		return
	}
	ids, err := a.historyIDs(s)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	allowed := false
	for _, id := range ids {
		allowed = allowed || id == job.SystemID
	}
	if !allowed {
		a.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	a.render(w, r, http.StatusOK, "job", &views.Page{Title: a.t(r, "job.kind."+job.Kind), Data: map[string]any{"System": s, "Job": job}})
}
