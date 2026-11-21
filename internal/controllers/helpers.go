package controllers

import (
	"crypto/subtle"
	"encoding/xml"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"itmon.nl/locutus/internal/auth"
	"itmon.nl/locutus/internal/i18n"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/views"
)

// t translates a message key for the language of the request.
func (a *App) t(r *http.Request, key string, args ...any) string {
	return a.tr.T(a.tr.Language(r), key, args...)
}

// render fills the common page fields and renders a template.
func (a *App) render(w http.ResponseWriter, r *http.Request, status int, name string, p *views.Page) {
	p.Lang = a.tr.Language(r)
	p.Languages = a.tr.Languages()
	if c, err := r.Cookie(i18n.Cookie); err == nil && a.tr.Has(c.Value) {
		p.LangChoice = c.Value
	}
	p.User = currentUser(r)
	p.CSRF = a.csrfToken(w, r)
	p.Flash = a.popFlash(w, r)
	p.Path = r.URL.Path
	a.view.Render(w, status, name, p)
}

func (a *App) errorPage(w http.ResponseWriter, r *http.Request, status int, key string) {
	a.render(w, r, status, "error", &views.Page{Title: a.t(r, "error.title"), Data: a.t(r, key)})
}

func (a *App) serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	a.errorPage(w, r, http.StatusInternalServerError, "error.internal")
}

// storeError maps model errors to an error page.
func (a *App) storeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, models.ErrNotFound) {
		a.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	a.serverError(w, r, err)
}

func pathID(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id
}

func formID(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.FormValue(name), 10, 64)
	return id
}

func formIDs(r *http.Request, name string) []int64 {
	var ids []int64
	for _, v := range r.Form[name] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func formText(r *http.Request, name string) string {
	return strings.TrimSpace(r.FormValue(name))
}

// secureCookies reports whether cookies should get the Secure flag.
func (a *App) secureCookies(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(a.cfg.BaseURL, "https://")
}

// baseURL returns the external base URL, used for OIDC and SAML callbacks.
func (a *App) baseURL(r *http.Request) string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (a *App) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.secureCookies(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// csrfToken returns the CSRF token of the browser, creating one if needed.
// Forms send it back in the "csrf" field (double submit cookie).
func (a *App) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 20 {
		return c.Value
	}
	tok := auth.RandomToken(24)
	a.setCookie(w, r, csrfCookie, tok, 0)
	return tok
}

// checkCSRF rejects POST requests without a valid token. The SAML assertion
// consumer is exempt because the IdP posts to it from another site; it is
// protected by the signed assertion and the request id instead.
func (a *App) checkCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !(strings.HasPrefix(r.URL.Path, "/auth/saml/") && strings.HasSuffix(r.URL.Path, "/acs")) {
			c, err := r.Cookie(csrfCookie)
			if err != nil || c.Value == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.FormValue("csrf"))) != 1 {
				a.errorPage(w, r, http.StatusForbidden, "error.csrf")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// flash stores a translated one-time message for the next page.
func (a *App) flash(w http.ResponseWriter, r *http.Request, kind, key string, args ...any) {
	a.setCookie(w, r, flashCookie, url.QueryEscape(kind+"|"+a.t(r, key, args...)), 60)
}

func (a *App) popFlash(w http.ResponseWriter, r *http.Request) *views.Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	a.setCookie(w, r, flashCookie, "", -1)
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		return nil
	}
	kind, msg, ok := strings.Cut(v, "|")
	if !ok || (kind != "success" && kind != "error") {
		return nil
	}
	return &views.Flash{Kind: kind, Message: msg}
}

// startSession logs the user in and sends the browser to the start page.
func (a *App) startSession(w http.ResponseWriter, r *http.Request, u *models.User) {
	token := auth.RandomToken(32)
	if err := a.store.CreateSession(token, u.ID, sessionTTL); err != nil {
		a.serverError(w, r, err)
		return
	}
	if err := a.store.TouchLogin(u.ID); err != nil {
		log.Printf("record login: %v", err)
	}
	a.setCookie(w, r, sessionCookie, token, int(sessionTTL.Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func xmlMarshal(v any) ([]byte, error) {
	out, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}
