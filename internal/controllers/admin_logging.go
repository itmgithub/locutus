package controllers

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/views"
)

// maxRetentionDays bounds the retention that can be set (about 100 years).
const maxRetentionDays = 36500

type loggingData struct {
	Stats  []models.LogStats
	Before string // default date of the manual cleanup: a week ago
}

// adminLogging shows the logging per category with its retention, and the
// form to delete old logging by hand.
func (a *App) adminLogging(w http.ResponseWriter, r *http.Request) {
	a.renderLogging(w, r, http.StatusOK, nil)
}

func (a *App) renderLogging(w http.ResponseWriter, r *http.Request, status int, errs []string) {
	stats, err := a.store.LogStats()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, status, "admin_logging", &views.Page{Title: a.t(r, "admin.logging.title"), Errors: errs,
		Data: loggingData{Stats: stats, Before: time.Now().AddDate(0, 0, -7).Format("2006-01-02")}})
}

// adminLoggingRetention stores how many days of logging are kept per category.
func (a *App) adminLoggingRetention(w http.ResponseWriter, r *http.Request) {
	days := map[string]int{}
	var errs []string
	for _, c := range models.LogCategories {
		v := strings.TrimSpace(r.FormValue("days_" + c))
		if v == "" {
			v = "0"
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxRetentionDays {
			errs = append(errs, a.t(r, "admin.logging.days_invalid", a.t(r, "admin.logging.category."+c)))
			continue
		}
		days[c] = n
	}
	if len(errs) > 0 {
		a.renderLogging(w, r, http.StatusUnprocessableEntity, errs)
		return
	}
	if err := a.store.SetLogRetention(days); err != nil {
		a.serverError(w, r, err)
		return
	}
	// Apply the new retention right away instead of within the hour.
	n, err := a.store.ApplyLogRetention(time.Now())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", "admin.logging.retention_saved", n)
	http.Redirect(w, r, "/admin/logging", http.StatusSeeOther)
}

// adminLoggingPurge deletes the logging of the chosen categories from
// before the chosen date.
func (a *App) adminLoggingPurge(w http.ResponseWriter, r *http.Request) {
	before, err := time.ParseInLocation("2006-01-02", r.FormValue("before"), time.Local)
	var errs []string
	if err != nil {
		errs = append(errs, a.t(r, "admin.logging.date_invalid"))
	}
	var categories []string
	for _, c := range r.Form["category"] {
		if models.ValidLogCategory(c) {
			categories = append(categories, c)
		}
	}
	if len(categories) == 0 {
		errs = append(errs, a.t(r, "admin.logging.category_required"))
	}
	if len(errs) > 0 {
		a.renderLogging(w, r, http.StatusUnprocessableEntity, errs)
		return
	}
	var total int64
	for _, c := range categories {
		n, err := a.store.PurgeLogs(c, before)
		total += n
		if err != nil {
			a.serverError(w, r, err)
			return
		}
	}
	log.Printf("logging before %s (%s) deleted by %s: %d entries", before.Format("2006-01-02"),
		strings.Join(categories, ", "), currentUser(r).Username, total)
	a.flash(w, r, "success", "admin.logging.purged", total, before.Format("02-01-2006"))
	http.Redirect(w, r, "/admin/logging", http.StatusSeeOther)
}
