package models

import (
	"strconv"

	"itmon.nl/locutus/internal/mail"
)

// Settings keys of the mail server. The password is stored encrypted.
const (
	mailHost     = "mail.host"
	mailPort     = "mail.port"
	mailSecurity = "mail.security"
	mailUsername = "mail.username"
	mailPassword = "mail.password"
	mailFrom     = "mail.from"
)

// MailConfig returns the mail server set up by the admin. Without one, Host
// is empty and Configured reports false.
func (st *Store) MailConfig() (*mail.Config, error) {
	get := func(key string) string {
		v, _ := st.Setting(key)
		return v
	}
	if _, err := st.Setting(mailHost); err != nil {
		return nil, err
	}
	c := &mail.Config{Host: get(mailHost), Security: get(mailSecurity), Username: get(mailUsername), From: get(mailFrom)}
	c.Port, _ = strconv.Atoi(get(mailPort))
	if c.Security == "" {
		c.Security = mail.StartTLS
	}
	if c.Port == 0 {
		c.Port = mail.DefaultPort(c.Security)
	}
	pw, err := st.box.Decrypt(get(mailPassword))
	if err != nil {
		return nil, err
	}
	c.Password = pw
	return c, nil
}

// MailConfigured reports whether a mail server has been set up.
func (st *Store) MailConfigured() bool {
	c, err := st.MailConfig()
	return err == nil && c.Configured()
}

// SaveMailConfig stores the mail server. A Host of "" removes the
// configuration, so that no mail is sent.
func (st *Store) SaveMailConfig(c *mail.Config) error {
	if c.Host == "" {
		c = &mail.Config{}
	}
	enc, err := st.box.Encrypt(c.Password)
	if err != nil {
		return err
	}
	port := ""
	if c.Host != "" {
		port = strconv.Itoa(c.Port)
	}
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]string{mailHost: c.Host, mailPort: port, mailSecurity: c.Security,
		mailUsername: c.Username, mailPassword: enc, mailFrom: c.From} {
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
			k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}
