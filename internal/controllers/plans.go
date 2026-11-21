package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"itmon.nl/locutus/internal/borg"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/schedule"
	"itmon.nl/locutus/internal/scheduler"
	"itmon.nl/locutus/internal/views"
)

// scheduleOption is one checkbox of the schedule editor.
type scheduleOption struct {
	Value   int
	Label   string
	Checked bool
}

// scheduleEditor is the web form representation of a schedule: a preset
// (every N minutes, hourly, daily, weekly, monthly) or a cron expression.
type scheduleEditor struct {
	Prefix          string
	PreviewURL      string
	Preset          schedule.Preset
	Kinds           []string
	MinuteIntervals []int
	HourIntervals   []int
	Weekdays        []scheduleOption
	MonthDays       []scheduleOption
	Expr            string
	Description     string
	Next            []time.Time
}

func (a *App) newScheduleEditor(r *http.Request, systemID int64, prefix string, p schedule.Preset) scheduleEditor {
	ed := scheduleEditor{
		Prefix:          prefix,
		PreviewURL:      "/systems/" + itoa(systemID) + "/schedule/preview?prefix=" + prefix,
		Preset:          p,
		Kinds:           schedule.Kinds,
		MinuteIntervals: schedule.MinuteIntervals,
		HourIntervals:   schedule.HourIntervals,
	}
	for _, d := range []int{1, 2, 3, 4, 5, 6, 0} { // Monday first
		ed.Weekdays = append(ed.Weekdays, scheduleOption{Value: d, Label: a.t(r, "weekday."+strconv.Itoa(d)),
			Checked: containsInt(p.Weekdays, d)})
	}
	for d := 1; d <= 31; d++ {
		ed.MonthDays = append(ed.MonthDays, scheduleOption{Value: d, Label: strconv.Itoa(d), Checked: containsInt(p.MonthDays, d)})
	}
	if expr, err := p.Expr(); err == nil {
		ed.Expr = expr
		ed.Description = a.describeSchedule(r, p)
		if spec, err := schedule.Parse(expr); err == nil {
			ed.Next = spec.NextN(time.Now(), 5)
		}
	}
	if ed.Preset.Cron == "" {
		ed.Preset.Cron = ed.Expr
	}
	if ed.Preset.Every == 0 {
		ed.Preset.Every = 1
	}
	return ed
}

func containsInt(vals []int, v int) bool {
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}

// listText joins items as "a, b en c".
func (a *App) listText(r *http.Request, items []string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + a.t(r, "schedule.and") + " " + items[len(items)-1]
}

// describeSchedule returns a sentence such as "Elke dag om 04:33".
func (a *App) describeSchedule(r *http.Request, p schedule.Preset) string {
	switch p.Kind {
	case schedule.KindMinutes:
		if p.Every == 1 {
			return a.t(r, "schedule.desc.every_minute")
		}
		return a.t(r, "schedule.desc.minutes", p.Every)
	case schedule.KindHourly:
		if p.Every == 1 {
			return a.t(r, "schedule.desc.hourly", p.Minute)
		}
		return a.t(r, "schedule.desc.hours", p.Every, p.Minute, p.Minute)
	case schedule.KindDaily:
		return a.t(r, "schedule.desc.daily", p.Time())
	case schedule.KindWeekly:
		var days []string
		for _, d := range []int{1, 2, 3, 4, 5, 6, 0} {
			if containsInt(p.Weekdays, d) {
				days = append(days, a.t(r, "weekday.long."+strconv.Itoa(d)))
			}
		}
		return a.t(r, "schedule.desc.weekly", a.listText(r, days), p.Time())
	case schedule.KindMonthly:
		var days []string
		for _, d := range p.MonthDays {
			days = append(days, strconv.Itoa(d))
		}
		return a.t(r, "schedule.desc.monthly", a.listText(r, days), p.Time())
	}
	return a.t(r, "schedule.desc.custom")
}

// describeExpr describes a stored cron expression.
func (a *App) describeExpr(r *http.Request, expr string) string {
	return a.describeSchedule(r, schedule.Detect(expr))
}

func formInt(r *http.Request, name string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
	if err != nil {
		return def
	}
	return n
}

func formInts(r *http.Request, name string) []int {
	var out []int
	for _, v := range r.Form[name] {
		if n, err := strconv.Atoi(v); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// presetFromForm reads the schedule editor with the given prefix.
func presetFromForm(r *http.Request, prefix string) (schedule.Preset, string, error) {
	p := schedule.Preset{Kind: r.FormValue(prefix + "_kind"), Every: 1, Cron: r.FormValue(prefix + "_cron")}
	switch p.Kind {
	case schedule.KindMinutes:
		p.Every = formInt(r, prefix+"_every_minutes", -1)
	case schedule.KindHourly:
		p.Every = formInt(r, prefix+"_every_hours", -1)
		p.Minute = formInt(r, prefix+"_minute", -1)
	case schedule.KindDaily, schedule.KindWeekly, schedule.KindMonthly:
		h, m, ok := strings.Cut(strings.TrimSpace(r.FormValue(prefix+"_time")), ":")
		p.Hour, p.Minute = -1, -1
		if ok {
			p.Hour, _ = strconv.Atoi(h)
			if n, err := strconv.Atoi(m); err == nil {
				p.Minute = n
			}
		}
		p.Weekdays = formInts(r, prefix+"_weekday")
		p.MonthDays = formInts(r, prefix+"_monthday")
	}
	expr, err := p.Expr()
	return p, expr, err
}

// schedulePreview returns the cron expression, a description and the next
// run times of the schedule editor, so the page can show them while editing.
func (a *App) schedulePreview(w http.ResponseWriter, r *http.Request) {
	if a.planClient(w, r) == nil {
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if prefix != "backup" && prefix != "prune" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	p, expr, err := presetFromForm(r, prefix)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"error": a.t(r, "schedule.invalid")})
		return
	}
	spec, _ := schedule.Parse(expr)
	var next []string
	for _, t := range spec.NextN(time.Now(), 5) {
		next = append(next, a.t(r, "weekday."+strconv.Itoa(int(t.Weekday())))+" "+t.Format("02-01-2006 15:04"))
	}
	json.NewEncoder(w).Encode(map[string]any{"expr": expr, "description": a.describeSchedule(r, p), "next": next})
}

// planClient loads a client system assigned to the current user.
func (a *App) planClient(w http.ResponseWriter, r *http.Request) *models.System {
	s := a.userSystem(w, r)
	if s == nil {
		return nil
	}
	if !s.IsClient() {
		a.errorPage(w, r, http.StatusBadRequest, "error.not_client")
		return nil
	}
	return s
}

type planData struct {
	System       *models.System
	Plan         *models.BackupPlan
	Schedule     scheduleEditor
	Compressions []string
}

func (a *App) renderPlan(w http.ResponseWriter, r *http.Request, status int, page string, s *models.System,
	p *models.BackupPlan, preset schedule.Preset, errs []string) {
	prefix, title := "backup", "plan.backup_title"
	if page == "prune_schedule" {
		prefix, title = "prune", "plan.prune_title"
	}
	data := planData{System: s, Plan: p, Compressions: borg.CompressionModes,
		Schedule: a.newScheduleEditor(r, s.ID, prefix, preset)}
	a.render(w, r, status, page, &views.Page{Title: a.t(r, title, s.Name), Data: data, Errors: errs})
}

func (a *App) loadPlan(w http.ResponseWriter, r *http.Request) (*models.System, *models.BackupPlan) {
	s := a.planClient(w, r)
	if s == nil {
		return nil, nil
	}
	p, err := a.store.GetPlan(s.ID)
	if err != nil {
		a.serverError(w, r, err)
		return nil, nil
	}
	return s, p
}

// unsafePathChars may not appear in paths and patterns: Locutus quotes them
// for the remote shell, and the client-side wrapper rejects these characters.
const unsafePathChars = "'\"`$\\;&|<>(){}\n\r"

func parseLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (a *App) backupPlanForm(w http.ResponseWriter, r *http.Request) {
	if s, p := a.loadPlan(w, r); s != nil {
		a.renderPlan(w, r, http.StatusOK, "backup_schedule", s, p, schedule.Detect(p.Schedule), nil)
	}
}

// backupPlanSubmit stores what is backed up and when.
func (a *App) backupPlanSubmit(w http.ResponseWriter, r *http.Request) {
	s, p := a.loadPlan(w, r)
	if s == nil {
		return
	}
	var errs []string
	p.Enabled = r.FormValue("enabled") == "1"
	p.Compression = r.FormValue("compression")
	p.OneFileSystem = r.FormValue("one_file_system") == "1"
	p.Paths = parseLines(r.FormValue("paths"))
	p.Excludes = parseLines(r.FormValue("excludes"))
	for _, path := range p.Paths {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, unsafePathChars) {
			errs = append(errs, a.t(r, "error.path_invalid", path))
		}
	}
	for _, ex := range p.Excludes {
		if strings.HasPrefix(ex, "-") || strings.ContainsAny(ex, unsafePathChars) {
			errs = append(errs, a.t(r, "error.exclude_invalid", ex))
		}
	}
	if p.Enabled && len(p.Paths) == 0 {
		errs = append(errs, a.t(r, "error.paths_required"))
	}
	if !borg.ValidCompression(p.Compression) {
		errs = append(errs, a.t(r, "error.compression_invalid"))
	}
	preset, expr, err := presetFromForm(r, "backup")
	if err != nil {
		errs = append(errs, a.t(r, "error.schedule_invalid"))
	}
	if len(errs) > 0 {
		a.renderPlan(w, r, http.StatusUnprocessableEntity, "backup_schedule", s, p, preset, errs)
		return
	}
	p.Schedule = expr
	p.NextBackupAt = time.Time{}
	if p.Enabled {
		p.NextBackupAt = schedule.NextRun(p.Schedule, time.Now())
	}
	a.savePlan(w, r, s, p, "plan.backup_saved")
}

func (a *App) prunePlanForm(w http.ResponseWriter, r *http.Request) {
	if s, p := a.loadPlan(w, r); s != nil {
		a.renderPlan(w, r, http.StatusOK, "prune_schedule", s, p, schedule.Detect(p.PruneSchedule), nil)
	}
}

// prunePlanSubmit stores which backups are kept and when old ones are removed.
func (a *App) prunePlanSubmit(w http.ResponseWriter, r *http.Request) {
	s, p := a.loadPlan(w, r)
	if s == nil {
		return
	}
	var errs []string
	p.PruneEnabled = r.FormValue("prune_enabled") == "1"
	keep := func(name string, dst *int) {
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
		if err != nil || n < 0 || n > 10000 {
			errs = append(errs, a.t(r, "error.keep_invalid", a.t(r, "plan."+name)))
			return
		}
		*dst = n
	}
	keep("keep_hourly", &p.KeepHourly)
	keep("keep_daily", &p.KeepDaily)
	keep("keep_weekly", &p.KeepWeekly)
	keep("keep_monthly", &p.KeepMonthly)
	keep("keep_yearly", &p.KeepYearly)
	if p.PruneEnabled && p.KeepHourly+p.KeepDaily+p.KeepWeekly+p.KeepMonthly+p.KeepYearly == 0 {
		errs = append(errs, a.t(r, "error.keep_required"))
	}
	preset, expr, err := presetFromForm(r, "prune")
	if err != nil {
		errs = append(errs, a.t(r, "error.schedule_invalid"))
	}
	if len(errs) > 0 {
		a.renderPlan(w, r, http.StatusUnprocessableEntity, "prune_schedule", s, p, preset, errs)
		return
	}
	p.PruneSchedule = expr
	p.NextPruneAt = time.Time{}
	if p.PruneEnabled {
		p.NextPruneAt = schedule.NextRun(p.PruneSchedule, time.Now())
	}
	a.savePlan(w, r, s, p, "plan.prune_saved")
}

func (a *App) savePlan(w http.ResponseWriter, r *http.Request, s *models.System, p *models.BackupPlan, msg string) {
	p.UpdatedBy = currentUser(r).Username
	if err := a.store.SavePlan(p); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", msg)
	http.Redirect(w, r, "/systems/"+itoa(s.ID), http.StatusSeeOther)
}

// archiveDelete deletes one archive of a client (backuppers only). It runs as
// a job of the client, so it never overlaps with a backup or prune.
func (a *App) archiveDelete(w http.ResponseWriter, r *http.Request) {
	s := a.planClient(w, r)
	if s == nil {
		return
	}
	archive, ok := a.archiveFromPath(w, r, s)
	if !ok {
		return
	}
	job, err := a.sched.StartDelete(s.ID, archive, currentUser(r).Username)
	if errors.Is(err, scheduler.ErrBusy) {
		a.flash(w, r, "error", "error.job_running")
		http.Redirect(w, r, "/systems/"+itoa(s.ID), http.StatusSeeOther)
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/systems/"+itoa(s.ID)+"/jobs/"+itoa(job.ID), http.StatusSeeOther)
}

// planRunNow starts a backup or prune of a client immediately.
func (a *App) planRunNow(w http.ResponseWriter, r *http.Request) {
	s, p := a.loadPlan(w, r)
	if s == nil {
		return
	}
	kind := r.PathValue("kind")
	if kind != models.JobBackup && kind != models.JobPrune {
		a.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	if kind == models.JobBackup && len(p.Paths) == 0 {
		a.flash(w, r, "error", "error.paths_required")
		http.Redirect(w, r, "/systems/"+itoa(s.ID)+"/schedule/backup", http.StatusSeeOther)
		return
	}
	job, err := a.sched.Start(s.ID, kind, models.TriggerManual, currentUser(r).Username)
	if errors.Is(err, scheduler.ErrBusy) {
		a.flash(w, r, "error", "error.job_running")
		http.Redirect(w, r, "/systems/"+itoa(s.ID), http.StatusSeeOther)
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/systems/"+itoa(s.ID)+"/jobs/"+itoa(job.ID), http.StatusSeeOther)
}
