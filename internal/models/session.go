package models

import "time"

// CreateSession stores a new session token for a user.
func (s *Store) CreateSession(token string, userID int64, ttl time.Duration) error {
	_, err := s.db.Exec(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`,
		token, userID, time.Now().Add(ttl).Unix())
	return err
}

// SessionUser returns the user of a valid, non-expired session.
func (s *Store) SessionUser(token string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+userFrom+`
		JOIN sessions s ON s.user_id = u.id WHERE s.token = ? AND s.expires_at > ?`, token, now()))
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// AuthState holds the state of a pending OIDC or SAML login.
type AuthState struct {
	State      string
	ProviderID int64
	Nonce      string
	RequestID  string
}

// CreateAuthState stores a pending external login.
func (s *Store) CreateAuthState(st AuthState, ttl time.Duration) error {
	_, err := s.db.Exec(`INSERT INTO auth_states (state, provider_id, nonce, request_id, expires_at) VALUES (?, ?, ?, ?, ?)`,
		st.State, st.ProviderID, st.Nonce, st.RequestID, time.Now().Add(ttl).Unix())
	return err
}

// ConsumeAuthState returns and deletes a pending external login.
func (s *Store) ConsumeAuthState(state string) (*AuthState, error) {
	var st AuthState
	err := s.db.QueryRow(`SELECT state, provider_id, nonce, request_id FROM auth_states WHERE state = ? AND expires_at > ?`,
		state, now()).Scan(&st.State, &st.ProviderID, &st.Nonce, &st.RequestID)
	if err != nil {
		return nil, notFound(err)
	}
	_, err = s.db.Exec(`DELETE FROM auth_states WHERE state = ?`, state)
	return &st, err
}

// PurgeExpired removes expired sessions and pending logins.
func (s *Store) PurgeExpired() error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM auth_states WHERE expires_at <= ?`, now())
	return err
}
