package controllers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"itmon.nl/locutus/internal/auth"
	"itmon.nl/locutus/internal/mail"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/views"
)

const authStateTTL = 10 * time.Minute

type userForm struct {
	Username    string
	DisplayName string
	Email       string
	Role        string
	Roles       []string
}

// validatePassword returns the translation keys of password problems.
func validatePassword(password, repeat string) []string {
	var errs []string
	if len(password) < auth.MinPasswordLength {
		errs = append(errs, "error.password_short")
	}
	if password != repeat {
		errs = append(errs, "error.password_mismatch")
	}
	return errs
}

func (a *App) translateAll(r *http.Request, keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		if k == "error.password_short" {
			out[i] = a.t(r, k, auth.MinPasswordLength)
		} else {
			out[i] = a.t(r, k)
		}
	}
	return out
}

func (a *App) setupForm(w http.ResponseWriter, r *http.Request) {
	if a.setupDone.Load() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, http.StatusOK, "setup", &views.Page{Title: a.t(r, "setup.title"), Data: userForm{}})
}

// setupSubmit creates the first admin user with a self-chosen name.
func (a *App) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if a.setupDone.Load() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	form := userForm{Username: formText(r, "username"), DisplayName: formText(r, "display_name"), Email: formText(r, "email")}
	errs := validatePassword(r.FormValue("password"), r.FormValue("password2"))
	if form.Username == "" {
		errs = append(errs, "error.username_required")
	}
	if len(errs) > 0 {
		a.render(w, r, http.StatusUnprocessableEntity, "setup", &views.Page{Title: a.t(r, "setup.title"), Data: form, Errors: a.translateAll(r, errs)})
		return
	}
	hash, err := auth.HashPassword(r.FormValue("password"))
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	// Re-check inside the request to avoid creating two initial admins.
	if n, err := a.store.CountUsers(); err != nil || n > 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u, err := a.store.CreateLocalUser(form.Username, form.DisplayName, form.Email, hash, models.RoleAdmin)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.setupDone.Store(true)
	log.Printf("initial admin user %q created", u.Username)
	a.startSession(w, r, u)
}

type loginData struct {
	Username  string
	Providers []*models.IdentityProvider
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request, status int, username string, errs []string) {
	providers, err := a.store.ListProviders(true)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, status, "login", &views.Page{Title: a.t(r, "login.title"), Errors: errs,
		Data: loginData{Username: username, Providers: providers}})
}

func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.loginPage(w, r, http.StatusOK, "", nil)
}

func (a *App) loginSubmit(w http.ResponseWriter, r *http.Request) {
	username := formText(r, "username")
	u, err := a.store.GetLocalUser(username)
	if err != nil {
		auth.CheckPasswordTiming()
	}
	if err != nil || !auth.CheckPassword(u.PasswordHash, r.FormValue("password")) {
		if err != nil && !errors.Is(err, models.ErrNotFound) {
			log.Printf("login: %v", err)
		}
		a.loginPage(w, r, http.StatusUnauthorized, username, []string{a.t(r, "error.login_failed")})
		return
	}
	a.startSession(w, r, u)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.store.DeleteSession(c.Value)
	}
	a.setCookie(w, r, sessionCookie, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) accountForm(w http.ResponseWriter, r *http.Request) {
	if !currentUser(r).IsLocal() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, http.StatusOK, "account", &views.Page{Title: a.t(r, "account.title")})
}

// accountSubmit lets a local user change the own password.
func (a *App) accountSubmit(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !u.IsLocal() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	errs := validatePassword(r.FormValue("password"), r.FormValue("password2"))
	if !auth.CheckPassword(u.PasswordHash, r.FormValue("current")) {
		errs = append([]string{"error.current_password"}, errs...)
	}
	if len(errs) > 0 {
		a.render(w, r, http.StatusUnprocessableEntity, "account", &views.Page{Title: a.t(r, "account.title"), Errors: a.translateAll(r, errs)})
		return
	}
	hash, err := auth.HashPassword(r.FormValue("password"))
	if err == nil {
		err = a.store.SetUserPassword(u.ID, hash)
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", "account.changed")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// accountEmailSubmit lets a local user change the own email address, to
// which the output of the jobs they start is mailed by default. Users of an
// identity provider get their address from the provider.
func (a *App) accountEmailSubmit(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !u.IsLocal() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	email := formText(r, "email")
	if email != "" {
		addr, err := mail.ParseAddress(email)
		if err != nil {
			a.render(w, r, http.StatusUnprocessableEntity, "account", &views.Page{Title: a.t(r, "account.title"),
				Errors: []string{a.t(r, "error.email_invalid")}})
			return
		}
		email = addr
	}
	if err := a.store.SetUserEmail(u.ID, email); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", "account.email_changed")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// enabledProvider loads an active provider of the expected type from the path.
func (a *App) enabledProvider(w http.ResponseWriter, r *http.Request, typ string) *models.IdentityProvider {
	p, err := a.store.GetProvider(pathID(r, "id"))
	if err != nil || p.Type != typ || !p.Enabled {
		a.errorPage(w, r, http.StatusNotFound, "error.provider_unavailable")
		return nil
	}
	return p
}

func (a *App) oidcRedirectURL(r *http.Request, p *models.IdentityProvider) string {
	return a.baseURL(r) + "/auth/oidc/" + itoa(p.ID) + "/callback"
}

func (a *App) oidcLogin(w http.ResponseWriter, r *http.Request) {
	p := a.enabledProvider(w, r, models.ProviderOIDC)
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	client, err := auth.NewOIDCClient(ctx, p.OIDC, a.oidcRedirectURL(r, p))
	if err != nil {
		a.externalLoginError(w, r, p, err)
		return
	}
	st := models.AuthState{State: auth.RandomToken(24), ProviderID: p.ID, Nonce: auth.RandomToken(24)}
	if err := a.store.CreateAuthState(st, authStateTTL); err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, client.AuthURL(st.State, st.Nonce), http.StatusFound)
}

func (a *App) oidcCallback(w http.ResponseWriter, r *http.Request) {
	p := a.enabledProvider(w, r, models.ProviderOIDC)
	if p == nil {
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		a.externalLoginError(w, r, p, errors.New(e+": "+r.URL.Query().Get("error_description")))
		return
	}
	st, err := a.store.ConsumeAuthState(r.URL.Query().Get("state"))
	if err != nil || st.ProviderID != p.ID {
		a.externalLoginError(w, r, p, errors.New("unknown or expired state"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	client, err := auth.NewOIDCClient(ctx, p.OIDC, a.oidcRedirectURL(r, p))
	if err != nil {
		a.externalLoginError(w, r, p, err)
		return
	}
	id, err := client.Exchange(ctx, r.URL.Query().Get("code"), st.Nonce)
	if err != nil {
		a.externalLoginError(w, r, p, err)
		return
	}
	a.externalLogin(w, r, p, id)
}

func (a *App) samlURLs(r *http.Request, p *models.IdentityProvider) (metadataURL, acsURL string) {
	base := a.baseURL(r) + "/auth/saml/" + itoa(p.ID)
	return base + "/metadata", base + "/acs"
}

func (a *App) samlLogin(w http.ResponseWriter, r *http.Request) {
	p := a.enabledProvider(w, r, models.ProviderSAML)
	if p == nil {
		return
	}
	mdURL, acsURL := a.samlURLs(r, p)
	sp, err := auth.NewSAMLServiceProvider(r.Context(), p.SAML, mdURL, acsURL, true)
	if err != nil {
		a.externalLoginError(w, r, p, err)
		return
	}
	state := auth.RandomToken(24)
	redirect, requestID, err := auth.SAMLAuthRequestURL(sp, state)
	if err != nil {
		a.externalLoginError(w, r, p, err)
		return
	}
	if err := a.store.CreateAuthState(models.AuthState{State: state, ProviderID: p.ID, RequestID: requestID}, authStateTTL); err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

// samlACS receives the SAML response posted by the IdP. The RelayState
// refers to the pending login, which holds the expected request id.
func (a *App) samlACS(w http.ResponseWriter, r *http.Request) {
	p := a.enabledProvider(w, r, models.ProviderSAML)
	if p == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		a.externalLoginError(w, r, p, err)
		return
	}
	st, err := a.store.ConsumeAuthState(r.PostForm.Get("RelayState"))
	if err != nil || st.ProviderID != p.ID {
		a.externalLoginError(w, r, p, errors.New("unknown or expired RelayState"))
		return
	}
	mdURL, acsURL := a.samlURLs(r, p)
	sp, err := auth.NewSAMLServiceProvider(r.Context(), p.SAML, mdURL, acsURL, true)
	if err != nil {
		a.externalLoginError(w, r, p, err)
		return
	}
	assertion, err := sp.ParseResponse(r, []string{st.RequestID})
	if err != nil {
		a.externalLoginError(w, r, p, err)
		return
	}
	a.externalLogin(w, r, p, auth.SAMLIdentity(assertion, p.SAML.UsernameAttribute))
}

// samlMetadata publishes the service provider metadata for the IdP administrator.
func (a *App) samlMetadata(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.GetProvider(pathID(r, "id"))
	if err != nil || p.Type != models.ProviderSAML {
		http.NotFound(w, r)
		return
	}
	mdURL, acsURL := a.samlURLs(r, p)
	sp, err := auth.NewSAMLServiceProvider(r.Context(), p.SAML, mdURL, acsURL, false)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	out, err := xmlMarshal(sp.Metadata())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	w.Write(out)
}

// externalLogin registers (or updates) the external user and logs it in.
// New users have no role until an admin assigns one.
func (a *App) externalLogin(w http.ResponseWriter, r *http.Request, p *models.IdentityProvider, id *auth.Identity) {
	if strings.TrimSpace(id.Subject) == "" {
		a.externalLoginError(w, r, p, errors.New("identity without subject"))
		return
	}
	u, err := a.store.UpsertExternalUser(p.ID, id.Subject, id.Username, id.DisplayName, id.Email)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.startSession(w, r, u)
}

func (a *App) externalLoginError(w http.ResponseWriter, r *http.Request, p *models.IdentityProvider, err error) {
	log.Printf("login via %s %q failed: %v", p.Type, p.Name, err)
	a.loginPage(w, r, http.StatusUnauthorized, "", []string{a.t(r, "error.external_login", p.Name)})
}
