package controllers

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"

	"itmon.nl/locutus/internal/mail"
	"itmon.nl/locutus/internal/tlsutil"
	"itmon.nl/locutus/internal/views"
)

type mailData struct {
	Config      *mail.Config
	Securities  []string
	HasPassword bool
	TestTo      string // default address of the test mail: the admin
}

// adminMail shows the mail server through which the output of jobs is mailed.
func (a *App) adminMail(w http.ResponseWriter, r *http.Request) {
	c, err := a.store.MailConfig()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.renderMail(w, r, http.StatusOK, c, c.Password != "", nil)
}

func (a *App) renderMail(w http.ResponseWriter, r *http.Request, status int, c *mail.Config, hasPassword bool, errs []string) {
	a.render(w, r, status, "admin_mail", &views.Page{Title: a.t(r, "admin.mail.title"), Errors: errs,
		Data: mailData{Config: c, Securities: mail.Securities, HasPassword: hasPassword, TestTo: currentUser(r).Email}})
}

// adminMailSubmit stores the mail server; an empty host removes it.
func (a *App) adminMailSubmit(w http.ResponseWriter, r *http.Request) {
	old, err := a.store.MailConfig()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	c := &mail.Config{Host: formText(r, "host"), Security: r.FormValue("security"), Username: formText(r, "username"),
		From: formText(r, "from")}
	c.Port, _ = strconv.Atoi(formText(r, "port"))
	if c.Port == 0 {
		c.Port = mail.DefaultPort(c.Security)
	}
	// The password is never shown: an empty field keeps the stored one.
	switch {
	case r.FormValue("remove_password") == "1" || c.Username == "":
	case r.FormValue("password") != "":
		c.Password = r.FormValue("password")
	default:
		c.Password = old.Password
	}
	if c.Host == "" {
		if err := a.store.SaveMailConfig(c); err != nil {
			a.serverError(w, r, err)
			return
		}
		a.flash(w, r, "success", "admin.mail.removed")
		http.Redirect(w, r, "/admin/mail", http.StatusSeeOther)
		return
	}
	if err := c.Validate(); err != nil {
		a.renderMail(w, r, http.StatusUnprocessableEntity, c, old.Password != "", []string{a.t(r, "admin.mail.invalid", err.Error())})
		return
	}
	if err := a.store.SaveMailConfig(c); err != nil {
		a.serverError(w, r, err)
		return
	}
	log.Printf("mail server set to %s:%d (%s) by %s", c.Host, c.Port, c.Security, currentUser(r).Username)
	a.flash(w, r, "success", "admin.mail.saved")
	http.Redirect(w, r, "/admin/mail", http.StatusSeeOther)
}

// adminMailTest sends a test mail through the stored mail server.
func (a *App) adminMailTest(w http.ResponseWriter, r *http.Request) {
	c, err := a.store.MailConfig()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if !c.Configured() {
		a.flash(w, r, "error", "admin.mail.not_configured")
		http.Redirect(w, r, "/admin/mail", http.StatusSeeOther)
		return
	}
	to, err := mail.ParseAddresses(r.FormValue("to"))
	if err != nil || len(to) == 0 {
		a.renderMail(w, r, http.StatusUnprocessableEntity, c, c.Password != "", []string{a.t(r, "error.email_invalid")})
		return
	}
	c.RootCAs = tlsutil.DefaultRootCAs()
	msg := mail.Message{To: to, Subject: "[Locutus] " + a.t(r, "admin.mail.test_subject"),
		Body: a.t(r, "admin.mail.test_body", currentUser(r).Username)}
	if err := mail.Send(context.WithoutCancel(r.Context()), c, msg); err != nil {
		log.Printf("test mail to %s: %v", strings.Join(to, ", "), err)
		a.renderMail(w, r, http.StatusBadGateway, c, c.Password != "", []string{a.t(r, "admin.mail.test_failed", err.Error())})
		return
	}
	a.flash(w, r, "success", "admin.mail.test_sent", strings.Join(to, ", "))
	http.Redirect(w, r, "/admin/mail", http.StatusSeeOther)
}

// notifyField is the "mail the output to" field of the forms that start a
// job. It is only shown when a mail server is configured, and filled in
// with the address of the user.
type notifyField struct {
	Enabled bool
	Default string
}

func (a *App) notifyField(r *http.Request) notifyField {
	if !a.store.MailConfigured() {
		return notifyField{}
	}
	return notifyField{Enabled: true, Default: currentUser(r).Email}
}

// notifyEmail reads the "notify_email" field of a form that starts a job:
// the normalised addresses, and false when they are invalid. Without a mail
// server nothing is mailed.
func (a *App) notifyEmail(r *http.Request) (string, bool) {
	if !a.store.MailConfigured() {
		return "", true
	}
	list, err := mail.ParseAddresses(r.FormValue("notify_email"))
	if err != nil {
		return "", false
	}
	return strings.Join(list, ", "), true
}
