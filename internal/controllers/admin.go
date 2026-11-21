package controllers

import (
	"net/http"
	"strings"

	"itmon.nl/locutus/internal/auth"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/views"
)

type usersData struct {
	Users []*models.User
	Roles []string
}

func (a *App) adminUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.store.ListUsers()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, http.StatusOK, "admin_users", &views.Page{Title: a.t(r, "admin.users.title"),
		Data: usersData{Users: users, Roles: models.Roles}})
}

func (a *App) adminNewUserForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "admin_user_new", &views.Page{Title: a.t(r, "admin.users.new"),
		Data: userForm{Role: models.RoleViewer, Roles: models.Roles}})
}

// adminNewUserSubmit creates a local user.
func (a *App) adminNewUserSubmit(w http.ResponseWriter, r *http.Request) {
	form := userForm{Username: formText(r, "username"), DisplayName: formText(r, "display_name"),
		Email: formText(r, "email"), Role: r.FormValue("role"), Roles: models.Roles}
	var errs []string
	if len(r.FormValue("password")) < auth.MinPasswordLength {
		errs = append(errs, "error.password_short")
	}
	if form.Username == "" {
		errs = append(errs, "error.username_required")
	} else if _, err := a.store.GetLocalUser(form.Username); err == nil {
		errs = append(errs, "error.username_taken")
	}
	if !models.ValidRole(form.Role) {
		errs = append(errs, "error.invalid_role")
	}
	if len(errs) > 0 {
		a.render(w, r, http.StatusUnprocessableEntity, "admin_user_new", &views.Page{Title: a.t(r, "admin.users.new"),
			Data: form, Errors: a.translateAll(r, errs)})
		return
	}
	hash, err := auth.HashPassword(r.FormValue("password"))
	if err == nil {
		_, err = a.store.CreateLocalUser(form.Username, form.DisplayName, form.Email, hash, form.Role)
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", "admin.users.created", form.Username)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// adminSetRole assigns a role to a local, OIDC or SAML user. An admin
// cannot remove the own admin role, so there is always at least one admin.
func (a *App) adminSetRole(w http.ResponseWriter, r *http.Request) {
	id, role := pathID(r, "id"), r.FormValue("role")
	if !models.ValidRole(role) {
		a.errorPage(w, r, http.StatusBadRequest, "error.invalid_role")
		return
	}
	u, err := a.store.GetUser(id)
	if err != nil {
		a.storeError(w, r, err)
		return
	}
	if u.ID == currentUser(r).ID && role != models.RoleAdmin {
		a.flash(w, r, "error", "admin.users.cannot_demote_self")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	if err := a.store.SetUserRole(id, role); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", "admin.users.role_changed", u.Username)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (a *App) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id == currentUser(r).ID {
		a.flash(w, r, "error", "admin.users.cannot_delete_self")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	u, err := a.store.GetUser(id)
	if err != nil {
		a.storeError(w, r, err)
		return
	}
	if err := a.store.DeleteUser(id); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", "admin.users.deleted", u.Username)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (a *App) adminProviders(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListProviders(false)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, http.StatusOK, "admin_providers", &views.Page{Title: a.t(r, "admin.providers.title"), Data: list})
}

type providerData struct {
	Provider    *models.IdentityProvider
	CallbackURL string
	MetadataURL string
}

// providerFromPath returns the provider being edited, or a new one of the
// type given in the path.
func (a *App) providerFromPath(w http.ResponseWriter, r *http.Request) *models.IdentityProvider {
	if typ := r.PathValue("type"); typ != "" {
		if typ != models.ProviderOIDC && typ != models.ProviderSAML {
			a.errorPage(w, r, http.StatusNotFound, "error.not_found")
			return nil
		}
		p := &models.IdentityProvider{Type: typ, Enabled: true}
		if typ == models.ProviderOIDC {
			p.OIDC.Scopes = "openid profile email"
		}
		return p
	}
	p, err := a.store.GetProvider(pathID(r, "id"))
	if err != nil {
		a.storeError(w, r, err)
		return nil
	}
	return p
}

func (a *App) renderProviderForm(w http.ResponseWriter, r *http.Request, status int, p *models.IdentityProvider, errs []string) {
	data := providerData{Provider: p}
	if p.Type == models.ProviderOIDC {
		data.CallbackURL = a.baseURL(r) + "/auth/oidc/{id}/callback"
		if p.ID != 0 {
			data.CallbackURL = a.oidcRedirectURL(r, p)
		}
	} else {
		data.CallbackURL = a.baseURL(r) + "/auth/saml/{id}/acs"
		if p.ID != 0 {
			data.MetadataURL, data.CallbackURL = a.samlURLs(r, p)
		}
	}
	a.render(w, r, status, "admin_provider_form", &views.Page{Title: a.t(r, "admin.providers.title"), Data: data, Errors: errs})
}

func (a *App) adminProviderForm(w http.ResponseWriter, r *http.Request) {
	if p := a.providerFromPath(w, r); p != nil {
		a.renderProviderForm(w, r, http.StatusOK, p, nil)
	}
}

// adminProviderSubmit creates or updates an OIDC or SAMLv2 configuration.
func (a *App) adminProviderSubmit(w http.ResponseWriter, r *http.Request) {
	p := a.providerFromPath(w, r)
	if p == nil {
		return
	}
	p.Name = formText(r, "name")
	p.Enabled = r.FormValue("enabled") == "1"
	var errs []string
	if p.Name == "" {
		errs = append(errs, "error.name_required")
	}
	if p.Type == models.ProviderOIDC {
		p.OIDC.IssuerURL = strings.TrimRight(formText(r, "issuer_url"), "/")
		p.OIDC.ClientID = formText(r, "client_id")
		if secret := r.FormValue("client_secret"); secret != "" {
			p.OIDC.ClientSecret = secret
		}
		p.OIDC.Scopes = formText(r, "scopes")
		p.OIDC.UsernameClaim = formText(r, "username_claim")
		if p.OIDC.IssuerURL == "" || p.OIDC.ClientID == "" {
			errs = append(errs, "error.oidc_required")
		}
	} else {
		p.SAML.IDPMetadataURL = formText(r, "idp_metadata_url")
		p.SAML.IDPMetadataXML = strings.TrimSpace(r.FormValue("idp_metadata_xml"))
		p.SAML.EntityID = formText(r, "entity_id")
		p.SAML.UsernameAttribute = formText(r, "username_attribute")
		if p.SAML.IDPMetadataURL == "" && p.SAML.IDPMetadataXML == "" {
			errs = append(errs, "error.saml_metadata_required")
		}
		if p.SAML.SPKeyPEM == "" {
			cert, key, err := auth.GenerateSAMLKeyPair("Locutus " + p.Name)
			if err != nil {
				a.serverError(w, r, err)
				return
			}
			p.SAML.SPCertificatePEM, p.SAML.SPKeyPEM = cert, key
		}
	}
	if len(errs) > 0 {
		a.renderProviderForm(w, r, http.StatusUnprocessableEntity, p, a.translateAll(r, errs))
		return
	}
	if err := a.store.SaveProvider(p); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", "admin.providers.saved", p.Name)
	if p.Type == models.ProviderSAML {
		// Show the form again so the admin can copy the SP metadata URL.
		http.Redirect(w, r, "/admin/providers/"+itoa(p.ID)+"/edit", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/providers", http.StatusSeeOther)
}

func (a *App) adminDeleteProvider(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.GetProvider(pathID(r, "id"))
	if err != nil {
		a.storeError(w, r, err)
		return
	}
	if currentUser(r).ProviderID == p.ID {
		a.flash(w, r, "error", "admin.providers.cannot_delete_own")
		http.Redirect(w, r, "/admin/providers", http.StatusSeeOther)
		return
	}
	if err := a.store.DeleteProvider(p.ID); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", "admin.providers.deleted", p.Name)
	http.Redirect(w, r, "/admin/providers", http.StatusSeeOther)
}

// adminSetPassword sets a new password for a local user.
func (a *App) adminSetPassword(w http.ResponseWriter, r *http.Request) {
	u, err := a.store.GetUser(pathID(r, "id"))
	if err != nil {
		a.storeError(w, r, err)
		return
	}
	if !u.IsLocal() {
		a.errorPage(w, r, http.StatusBadRequest, "error.not_local")
		return
	}
	if len(r.FormValue("password")) < auth.MinPasswordLength {
		a.flash(w, r, "error", "error.password_short", auth.MinPasswordLength)
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
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
	a.flash(w, r, "success", "admin.users.password_changed", u.Username)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
