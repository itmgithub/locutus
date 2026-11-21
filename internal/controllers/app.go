// Package controllers contains the HTTP handlers (the "controller" part of
// MVC). Handlers read input, call the models and services and render a view.
package controllers

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"itmon.nl/locutus/internal/borg"
	"itmon.nl/locutus/internal/config"
	"itmon.nl/locutus/internal/i18n"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/scheduler"
	"itmon.nl/locutus/internal/views"
)

const (
	sessionCookie = "locutus_session"
	csrfCookie    = "locutus_csrf"
	flashCookie   = "locutus_flash"
	sessionTTL    = 12 * time.Hour
)

type ctxKey int

const userKey ctxKey = 1

// App bundles the dependencies of all controllers.
type App struct {
	cfg   *config.Config
	store *models.Store
	view  *views.Renderer
	tr    *i18n.Translator
	borg  *borg.Service
	sched *scheduler.Scheduler
	// setupDone caches that the first admin has been created.
	setupDone atomic.Bool
}

// New creates the application controllers.
func New(cfg *config.Config, store *models.Store, view *views.Renderer, tr *i18n.Translator,
	b *borg.Service, sched *scheduler.Scheduler) *App {
	a := &App{cfg: cfg, store: store, view: view, tr: tr, borg: b, sched: sched}
	go a.purgeLoop()
	return a
}

// purgeLoop removes expired sessions and, according to the retention set by
// the admin, old logging; once at the start and then every hour.
func (a *App) purgeLoop() {
	for {
		if err := a.store.PurgeExpired(); err != nil {
			log.Printf("purge expired sessions: %v", err)
		}
		if n, err := a.store.ApplyLogRetention(time.Now()); err != nil {
			log.Printf("clean up logging: %v", err)
		} else if n > 0 {
			log.Printf("clean up logging: %d old entries deleted", n)
		}
		time.Sleep(time.Hour)
	}
}

// Routes returns the HTTP handler with all routes and middleware.
func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", views.Static())

	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /setup", a.setupForm)
	mux.HandleFunc("POST /setup", a.setupSubmit)
	mux.HandleFunc("GET /login", a.loginForm)
	mux.HandleFunc("POST /login", a.loginSubmit)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("POST /language", a.setLanguage)
	mux.HandleFunc("GET /norole", a.requireLogin(a.noRole))
	mux.HandleFunc("GET /account", a.requireLogin(a.accountForm))
	mux.HandleFunc("POST /account", a.requireLogin(a.accountSubmit))

	// External identity providers.
	mux.HandleFunc("GET /auth/oidc/{id}/login", a.oidcLogin)
	mux.HandleFunc("GET /auth/oidc/{id}/callback", a.oidcCallback)
	mux.HandleFunc("GET /auth/saml/{id}/login", a.samlLogin)
	mux.HandleFunc("POST /auth/saml/{id}/acs", a.samlACS)
	mux.HandleFunc("GET /auth/saml/{id}/metadata", a.samlMetadata)

	// Administration: identity providers, users, roles and systems.
	// The admin has no access to backups, files or restores.
	admin := func(h http.HandlerFunc) http.HandlerFunc { return a.requireRole(h, models.RoleAdmin) }
	mux.HandleFunc("GET /admin/users", admin(a.adminUsers))
	mux.HandleFunc("GET /admin/users/new", admin(a.adminNewUserForm))
	mux.HandleFunc("POST /admin/users/new", admin(a.adminNewUserSubmit))
	mux.HandleFunc("POST /admin/users/{id}/role", admin(a.adminSetRole))
	mux.HandleFunc("POST /admin/users/{id}/password", admin(a.adminSetPassword))
	mux.HandleFunc("POST /admin/users/{id}/delete", admin(a.adminDeleteUser))
	mux.HandleFunc("GET /admin/providers", admin(a.adminProviders))
	mux.HandleFunc("GET /admin/providers/{type}/new", admin(a.adminProviderForm))
	mux.HandleFunc("POST /admin/providers/{type}/new", admin(a.adminProviderSubmit))
	mux.HandleFunc("GET /admin/providers/{id}/edit", admin(a.adminProviderForm))
	mux.HandleFunc("POST /admin/providers/{id}/edit", admin(a.adminProviderSubmit))
	mux.HandleFunc("POST /admin/providers/{id}/delete", admin(a.adminDeleteProvider))
	mux.HandleFunc("GET /admin/systems", admin(a.adminSystems))
	mux.HandleFunc("GET /admin/systems/{kind}/new", admin(a.adminSystemForm))
	mux.HandleFunc("POST /admin/systems/{kind}/new", admin(a.adminSystemSubmit))
	mux.HandleFunc("GET /admin/systems/{id}/edit", admin(a.adminSystemForm))
	mux.HandleFunc("POST /admin/systems/{id}/edit", admin(a.adminSystemSubmit))
	mux.HandleFunc("POST /admin/systems/{id}/users", admin(a.adminSystemUsers))
	mux.HandleFunc("POST /admin/systems/{id}/genkey", admin(a.adminSystemGenerateKey))
	mux.HandleFunc("POST /admin/systems/{id}/test", admin(a.adminSystemTest))
	mux.HandleFunc("POST /admin/systems/{id}/init", admin(a.adminSystemInit))
	mux.HandleFunc("POST /admin/systems/{id}/delete", admin(a.adminDeleteSystem))
	mux.HandleFunc("GET /admin/logging", admin(a.adminLogging))
	mux.HandleFunc("POST /admin/logging/retention", admin(a.adminLoggingRetention))
	mux.HandleFunc("POST /admin/logging/purge", admin(a.adminLoggingPurge))

	// Backups of assigned systems: viewers see, restorers also download and
	// backuppers also maintain the backup and prune plans of clients.
	view := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireRole(h, models.RoleViewer, models.RoleRestorer, models.RoleBackupper)
	}
	restore := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireRole(h, models.RoleRestorer, models.RoleBackupper)
	}
	backup := func(h http.HandlerFunc) http.HandlerFunc { return a.requireRole(h, models.RoleBackupper) }
	mux.HandleFunc("GET /systems", view(a.systems))
	mux.HandleFunc("GET /systems/{id}", view(a.systemOverview))
	mux.HandleFunc("GET /systems/{id}/archives/{archive}", view(a.archiveBrowse))
	mux.HandleFunc("GET /systems/{id}/archives/{archive}/tree", view(a.archiveTree))
	mux.HandleFunc("POST /systems/{id}/archives/{archive}/download", restore(a.archiveDownload))
	mux.HandleFunc("POST /systems/{id}/archives/{archive}/restore", restore(a.archiveRestore))
	mux.HandleFunc("POST /systems/{id}/archives/{archive}/delete", backup(a.archiveDelete))
	mux.HandleFunc("GET /systems/{id}/jobs/{jid}", view(a.jobDetail))
	mux.HandleFunc("GET /systems/{id}/schedule/backup", backup(a.backupPlanForm))
	mux.HandleFunc("POST /systems/{id}/schedule/backup", backup(a.backupPlanSubmit))
	mux.HandleFunc("GET /systems/{id}/schedule/prune", backup(a.prunePlanForm))
	mux.HandleFunc("POST /systems/{id}/schedule/prune", backup(a.prunePlanSubmit))
	mux.HandleFunc("GET /systems/{id}/schedule/preview", backup(a.schedulePreview))
	mux.HandleFunc("POST /systems/{id}/run/{kind}", backup(a.planRunNow))

	return a.securityHeaders(a.loadSession(a.requireSetup(a.checkCSRF(mux))))
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; form-action 'self'; frame-ancestors 'none'")
		if a.secureCookies(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// loadSession puts the logged in user (if any) in the request context.
func (a *App) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			if u, err := a.store.SessionUser(c.Value); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userKey, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireSetup redirects every request to /setup until the first admin exists.
func (a *App) requireSetup(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.setupDone.Load() {
			n, err := a.store.CountUsers()
			if err != nil {
				a.serverError(w, r, err)
				return
			}
			if n > 0 {
				a.setupDone.Store(true)
			} else if r.URL.Path != "/setup" && !strings.HasPrefix(r.URL.Path, "/static/") {
				http.Redirect(w, r, "/setup", http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func currentUser(r *http.Request) *models.User {
	u, _ := r.Context().Value(userKey).(*models.User)
	return u
}

func (a *App) requireLogin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

// requireRole only lets users with one of the given roles through.
func (a *App) requireRole(h http.HandlerFunc, roles ...string) http.HandlerFunc {
	return a.requireLogin(func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		for _, role := range roles {
			if u.Role == role {
				h(w, r)
				return
			}
		}
		if u.Role == models.RoleNone {
			http.Redirect(w, r, "/norole", http.StatusSeeOther)
			return
		}
		a.errorPage(w, r, http.StatusForbidden, "error.forbidden")
	})
}

// home sends the user to the start page that fits the role.
func (a *App) home(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	switch {
	case u == nil:
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case u.Role == models.RoleAdmin:
		http.Redirect(w, r, "/admin/systems", http.StatusSeeOther)
	case u.CanView():
		http.Redirect(w, r, "/systems", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/norole", http.StatusSeeOther)
	}
}

// setLanguage stores the language chosen by the user in a cookie; an empty
// choice removes it, so that the language of the browser is used again.
func (a *App) setLanguage(w http.ResponseWriter, r *http.Request) {
	lang := r.FormValue("lang")
	switch {
	case lang == "":
		a.setCookie(w, r, i18n.Cookie, "", -1)
	case a.tr.Has(lang):
		a.setCookie(w, r, i18n.Cookie, lang, 365*24*3600)
	}
	next := r.FormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) noRole(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != models.RoleNone {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, http.StatusOK, "norole", &views.Page{Title: a.t(r, "norole.title")})
}
