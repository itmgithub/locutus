package models

import (
	"database/sql"
	"time"
)

// Roles a user can have. A user without a role can log in but cannot do anything.
//
//   - admin: manages identity providers, users, roles and systems; has no
//     access to backups or files.
//   - viewer: sees the backups, restores and archive contents of assigned systems.
//   - restorer: viewer + downloads files from archives.
//   - backupper: restorer + maintains backup and prune schedules of clients.
const (
	RoleNone      = ""
	RoleAdmin     = "admin"
	RoleViewer    = "viewer"
	RoleRestorer  = "restorer"
	RoleBackupper = "backupper"
)

// Roles lists all assignable roles.
var Roles = []string{RoleAdmin, RoleViewer, RoleRestorer, RoleBackupper}

// ValidRole reports whether role is a known role (including "no role").
func ValidRole(role string) bool {
	if role == RoleNone {
		return true
	}
	for _, r := range Roles {
		if r == role {
			return true
		}
	}
	return false
}

// User is a local user or a user authenticated by an external identity provider.
type User struct {
	ID           int64
	Username     string
	DisplayName  string
	Email        string
	PasswordHash string
	ProviderID   int64 // 0 for local users
	ProviderName string
	ProviderType string
	Subject      string
	Role         string
	CreatedAt    time.Time
	LastLoginAt  time.Time
}

// CanView reports whether the user may see backups of assigned systems.
func (u *User) CanView() bool {
	return u.Role == RoleViewer || u.Role == RoleRestorer || u.Role == RoleBackupper
}

// CanRestore reports whether the user may download files from archives.
func (u *User) CanRestore() bool { return u.Role == RoleRestorer || u.Role == RoleBackupper }

// CanBackup reports whether the user may maintain backup schedules.
func (u *User) CanBackup() bool { return u.Role == RoleBackupper }

// IsLocal reports whether the user authenticates with a local password.
func (u *User) IsLocal() bool { return u.ProviderID == 0 }

// Name returns the most friendly name available.
func (u *User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

const userColumns = `u.id, u.username, u.display_name, u.email, u.password_hash, COALESCE(u.provider_id, 0),
	COALESCE(p.name, ''), COALESCE(p.type, ''), u.subject, u.role, u.created_at, u.last_login_at`

const userFrom = ` FROM users u LEFT JOIN identity_providers p ON p.id = u.provider_id `

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created, lastLogin int64
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.PasswordHash, &u.ProviderID,
		&u.ProviderName, &u.ProviderType, &u.Subject, &u.Role, &created, &lastLogin); err != nil {
		return nil, notFound(err)
	}
	u.CreatedAt = time.Unix(created, 0)
	u.LastLoginAt = unixTime(lastLogin)
	return &u, nil
}

// CountUsers returns the total number of users.
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountAdmins returns the number of users with the admin role.
func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&n)
	return n, err
}

// ListUsers returns all users ordered by username.
func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + userFrom + ` ORDER BY u.username COLLATE NOCASE, p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// GetUser returns the user with the given id.
func (s *Store) GetUser(id int64) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+userFrom+` WHERE u.id = ?`, id))
}

// GetLocalUser returns the local user with the given username.
func (s *Store) GetLocalUser(username string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+userFrom+` WHERE u.provider_id IS NULL AND u.username = ?`, username))
}

// CreateLocalUser creates a user that logs in with a password.
func (s *Store) CreateLocalUser(username, displayName, email, passwordHash, role string) (*User, error) {
	res, err := s.db.Exec(`INSERT INTO users (username, display_name, email, password_hash, role, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, username, displayName, email, passwordHash, role, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetUser(id)
}

// UpsertExternalUser finds or creates the user for an external identity and
// refreshes its profile data. New external users get no role.
func (s *Store) UpsertExternalUser(providerID int64, subject, username, displayName, email string) (*User, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM users WHERE provider_id = ? AND subject = ?`, providerID, subject).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		res, err := s.db.Exec(`INSERT INTO users (username, display_name, email, provider_id, subject, role, created_at)
			VALUES (?, ?, ?, ?, ?, '', ?)`, username, displayName, email, providerID, subject, now())
		if err != nil {
			return nil, err
		}
		id, _ = res.LastInsertId()
	case err != nil:
		return nil, err
	default:
		if _, err := s.db.Exec(`UPDATE users SET username = ?, display_name = ?, email = ? WHERE id = ?`,
			username, displayName, email, id); err != nil {
			return nil, err
		}
	}
	return s.GetUser(id)
}

// SetUserRole changes the role of a user.
func (s *Store) SetUserRole(id int64, role string) error {
	_, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, id)
	return err
}

// SetUserPassword changes the password hash of a local user.
func (s *Store) SetUserPassword(id int64, passwordHash string) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ? AND provider_id IS NULL`, passwordHash, id)
	return err
}

// TouchLogin records the time of a successful login.
func (s *Store) TouchLogin(id int64) error {
	_, err := s.db.Exec(`UPDATE users SET last_login_at = ? WHERE id = ?`, now(), id)
	return err
}

// DeleteUser removes a user and its sessions.
func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}
