package models

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// System kinds and locations.
const (
	KindRepository = "repository"
	KindClient     = "client"

	LocationLocal  = "local"
	LocationRemote = "remote"
)

// System is a borg repository or a client that is backed up into a
// repository. The SSH key and passphrase are never loaded into this struct;
// use SystemSecrets for that, which only the borg layer needs.
type System struct {
	ID            int64
	Name          string
	Kind          string
	Location      string
	Description   string
	Host          string
	Port          int
	SSHUser       string
	RepoPath      string
	BorgVersion   int
	RepositoryID  int64 // clients only: the repository the client backs up to
	ArchivePrefix string
	HasSSHKey     bool
	HasPassphrase bool
	HasPruneKey   bool // repositories only: a separate key for prune and compact
	UseSudo       bool // remote clients only: run borg, tar and stat with "sudo -n"
	CreatedAt     time.Time

	// Repository is filled by GetSystem for client systems.
	Repository *System
}

// SystemSecrets holds the decrypted secrets of a system.
type SystemSecrets struct {
	SSHKey      string
	PruneSSHKey string // repositories only; "" means SSHKey is used for prune too
	Passphrase  string
}

// IsRepository reports whether the system is a borg repository.
func (s *System) IsRepository() bool { return s.Kind == KindRepository }

// IsClient reports whether the system is a client.
func (s *System) IsClient() bool { return s.Kind == KindClient }

// IsRemote reports whether the system is reached over SSH.
func (s *System) IsRemote() bool { return s.Location == LocationRemote }

// RepoLocation returns the borg repository location: the local path or an
// ssh:// URL. Relative remote paths are relative to the home directory.
func (s *System) RepoLocation() string {
	if !s.IsRemote() {
		return s.RepoPath
	}
	path := s.RepoPath
	switch {
	case strings.HasPrefix(path, "/"):
	case strings.HasPrefix(path, "~"):
		path = "/" + path
	default:
		path = "/./" + path
	}
	return "ssh://" + s.SSHUser + "@" + s.hostPort() + path
}

// SSHAddress returns host:port for an SSH connection.
func (s *System) SSHAddress() string { return s.hostPort() }

func (s *System) hostPort() string {
	host := s.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]" // IPv6 literal
	}
	port := s.Port
	if port == 0 {
		port = 22
	}
	return host + ":" + strconv.Itoa(port)
}

// ArchiveGlob returns the glob that matches the archives of a client, or ""
// (all archives) for a repository.
func (s *System) ArchiveGlob() string {
	if s.IsClient() && s.ArchivePrefix != "" {
		return s.ArchivePrefix + ".*"
	}
	return ""
}

var prefixInvalid = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// ArchivePrefixFor derives the archive name prefix of a client from its name.
// The prefix never contains a dot, which separates it from the timestamp.
func ArchivePrefixFor(name string) string {
	p := strings.Trim(prefixInvalid.ReplaceAllString(name, "_"), "_")
	if p == "" {
		p = "client"
	}
	return p
}

const systemColumns = `s.id, s.name, s.kind, s.location, s.description, s.host, s.port, s.ssh_user, s.repo_path,
	s.borg_version, COALESCE(s.repository_id, 0), s.archive_prefix, s.ssh_key != '', s.passphrase != '', s.prune_ssh_key != '', s.use_sudo, s.created_at`

func scanSystem(row interface{ Scan(...any) error }) (*System, error) {
	var s System
	var created int64
	if err := row.Scan(&s.ID, &s.Name, &s.Kind, &s.Location, &s.Description, &s.Host, &s.Port, &s.SSHUser,
		&s.RepoPath, &s.BorgVersion, &s.RepositoryID, &s.ArchivePrefix, &s.HasSSHKey, &s.HasPassphrase, &s.HasPruneKey, &s.UseSudo, &created); err != nil {
		return nil, notFound(err)
	}
	s.CreatedAt = time.Unix(created, 0)
	return &s, nil
}

func (st *Store) querySystems(q string, args ...any) ([]*System, error) {
	rows, err := st.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*System
	for rows.Next() {
		s, err := scanSystem(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

// ListSystems returns all systems, optionally only those of one kind.
func (st *Store) ListSystems(kind string) ([]*System, error) {
	q := `SELECT ` + systemColumns + ` FROM systems s`
	var args []any
	if kind != "" {
		q += ` WHERE s.kind = ?`
		args = append(args, kind)
	}
	return st.querySystems(q+` ORDER BY s.kind DESC, s.name`, args...)
}

// ListUserSystems returns the systems assigned to a user.
func (st *Store) ListUserSystems(userID int64) ([]*System, error) {
	return st.querySystems(`SELECT `+systemColumns+` FROM systems s
		JOIN system_users su ON su.system_id = s.id WHERE su.user_id = ? ORDER BY s.kind DESC, s.name`, userID)
}

// ListRepositoryClients returns the clients that back up into a repository.
func (st *Store) ListRepositoryClients(repoID int64) ([]*System, error) {
	return st.querySystems(`SELECT `+systemColumns+` FROM systems s WHERE s.repository_id = ? ORDER BY s.name`, repoID)
}

// GetSystem returns one system; for a client the repository is loaded too.
func (st *Store) GetSystem(id int64) (*System, error) {
	s, err := scanSystem(st.db.QueryRow(`SELECT `+systemColumns+` FROM systems s WHERE s.id = ?`, id))
	if err != nil {
		return nil, err
	}
	if s.IsClient() && s.RepositoryID != 0 {
		if s.Repository, err = scanSystem(st.db.QueryRow(`SELECT `+systemColumns+` FROM systems s WHERE s.id = ?`, s.RepositoryID)); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SystemNameTaken reports whether another system already uses name.
func (st *Store) SystemNameTaken(name string, exceptID int64) (bool, error) {
	var n int
	err := st.db.QueryRow(`SELECT COUNT(*) FROM systems WHERE name = ? AND id != ?`, name, exceptID).Scan(&n)
	return n > 0, err
}

// ArchivePrefixTaken reports whether another client already uses prefix.
func (st *Store) ArchivePrefixTaken(prefix string, exceptID int64) (bool, error) {
	var n int
	err := st.db.QueryRow(`SELECT COUNT(*) FROM systems WHERE kind = 'client' AND archive_prefix = ? COLLATE NOCASE AND id != ?`,
		prefix, exceptID).Scan(&n)
	return n > 0, err
}

// SaveSystem inserts (ID == 0) or updates a system; secrets are not touched.
func (st *Store) SaveSystem(s *System) error {
	if s.BorgVersion == 0 {
		s.BorgVersion = 1
	}
	if s.ID == 0 {
		res, err := st.db.Exec(`INSERT INTO systems (name, kind, location, description, host, port, ssh_user, repo_path,
			borg_version, repository_id, archive_prefix, use_sudo, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.Name, s.Kind, s.Location, s.Description, s.Host, s.Port, s.SSHUser, s.RepoPath,
			s.BorgVersion, nullID(s.RepositoryID), s.ArchivePrefix, s.UseSudo, now())
		if err != nil {
			return err
		}
		s.ID, err = res.LastInsertId()
		return err
	}
	_, err := st.db.Exec(`UPDATE systems SET name = ?, location = ?, description = ?, host = ?, port = ?, ssh_user = ?,
		repo_path = ?, borg_version = ?, repository_id = ?, archive_prefix = ?, use_sudo = ? WHERE id = ?`,
		s.Name, s.Location, s.Description, s.Host, s.Port, s.SSHUser, s.RepoPath, s.BorgVersion,
		nullID(s.RepositoryID), s.ArchivePrefix, s.UseSudo, s.ID)
	return err
}

// SetSystemSSHKey stores (encrypted) or, with "", removes the SSH private key.
func (st *Store) SetSystemSSHKey(id int64, key string) error {
	enc, err := st.box.Encrypt(key)
	if err != nil {
		return err
	}
	_, err = st.db.Exec(`UPDATE systems SET ssh_key = ? WHERE id = ?`, enc, id)
	return err
}

// SetSystemPruneSSHKey stores (encrypted) or, with "", removes the separate
// SSH key that a repository uses for prune and compact.
func (st *Store) SetSystemPruneSSHKey(id int64, key string) error {
	enc, err := st.box.Encrypt(key)
	if err != nil {
		return err
	}
	_, err = st.db.Exec(`UPDATE systems SET prune_ssh_key = ? WHERE id = ?`, enc, id)
	return err
}

// SetSystemPassphrase stores (encrypted) or, with "", removes the passphrase.
func (st *Store) SetSystemPassphrase(id int64, passphrase string) error {
	enc, err := st.box.Encrypt(passphrase)
	if err != nil {
		return err
	}
	_, err = st.db.Exec(`UPDATE systems SET passphrase = ? WHERE id = ?`, enc, id)
	return err
}

// SystemSecrets returns the decrypted SSH key and passphrase of a system.
func (st *Store) SystemSecrets(id int64) (*SystemSecrets, error) {
	var key, pruneKey, pass string
	if err := st.db.QueryRow(`SELECT ssh_key, prune_ssh_key, passphrase FROM systems WHERE id = ?`, id).Scan(&key, &pruneKey, &pass); err != nil {
		return nil, notFound(err)
	}
	var sec SystemSecrets
	var err error
	if sec.SSHKey, err = st.box.Decrypt(key); err != nil {
		return nil, err
	}
	if sec.PruneSSHKey, err = st.box.Decrypt(pruneKey); err != nil {
		return nil, err
	}
	if sec.Passphrase, err = st.box.Decrypt(pass); err != nil {
		return nil, err
	}
	return &sec, nil
}

// CountRepositoryClients returns how many clients use a repository.
func (st *Store) CountRepositoryClients(repoID int64) (int, error) {
	var n int
	err := st.db.QueryRow(`SELECT COUNT(*) FROM systems WHERE repository_id = ?`, repoID).Scan(&n)
	return n, err
}

// DeleteSystem removes a system with its plan, history and assignments.
func (st *Store) DeleteSystem(id int64) error {
	_, err := st.db.Exec(`DELETE FROM systems WHERE id = ?`, id)
	return err
}

// SystemUserIDs returns the ids of the users assigned to a system.
func (st *Store) SystemUserIDs(systemID int64) ([]int64, error) {
	rows, err := st.db.Query(`SELECT user_id FROM system_users WHERE system_id = ?`, systemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetSystemUsers replaces the users assigned to a system.
func (st *Store) SetSystemUsers(systemID int64, userIDs []int64) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM system_users WHERE system_id = ?`, systemID); err != nil {
		return err
	}
	for _, uid := range userIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO system_users (system_id, user_id) VALUES (?, ?)`, systemID, uid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UserHasSystem reports whether a system is assigned to a user.
func (st *Store) UserHasSystem(userID, systemID int64) (bool, error) {
	var x int
	err := st.db.QueryRow(`SELECT 1 FROM system_users WHERE user_id = ? AND system_id = ?`, userID, systemID).Scan(&x)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}
