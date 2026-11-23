package models

import (
	"strings"
	"time"
)

// Container is a Locutus client container: the systems with location
// LocationLocutusClient at one host and port. Locutus pushes their
// configuration to it with the management key.
type Container struct {
	Host          string
	Port          int
	HasManagerKey bool
	PushedAt      time.Time // zero: never pushed
	PushOK        bool
	PushOutput    string
}

// ContainerOf returns the container of a system in a Locutus client
// container (with what is known about it), or nil for other systems.
func (st *Store) ContainerOf(s *System) (*Container, error) {
	if !s.IsLocutusClient() {
		return nil, nil
	}
	c := &Container{Host: s.Host, Port: s.Port}
	var pushedAt int64
	err := st.db.QueryRow(`SELECT manager_key != '', pushed_at, push_ok, push_output FROM client_containers
		WHERE host = ? AND port = ?`, s.Host, s.Port).Scan(&c.HasManagerKey, &pushedAt, &c.PushOK, &c.PushOutput)
	if err = notFound(err); err != nil && err != ErrNotFound {
		return nil, err
	}
	c.PushedAt = unixTime(pushedAt)
	return c, nil
}

// SSHAddress returns host:port of the container.
func (c *Container) SSHAddress() string {
	return (&System{Host: c.Host, Port: c.Port}).SSHAddress()
}

// Key identifies the container, for example in a map.
func (c *Container) Key() string { return strings.ToLower(c.SSHAddress()) }

// ContainerSystems returns the systems in a container.
func (st *Store) ContainerSystems(c *Container) ([]*System, error) {
	return st.querySystems(`SELECT `+systemColumns+` FROM systems s
		WHERE s.client_container = 1 AND s.host = ? COLLATE NOCASE AND s.port = ? ORDER BY s.kind DESC, s.name`, c.Host, c.Port)
}

// SetContainerManagerKey stores (encrypted) the private management key of
// a container.
func (st *Store) SetContainerManagerKey(c *Container, key string) error {
	enc, err := st.box.Encrypt(key)
	if err != nil {
		return err
	}
	_, err = st.db.Exec(`INSERT INTO client_containers (host, port, manager_key) VALUES (?, ?, ?)
		ON CONFLICT (host, port) DO UPDATE SET manager_key = excluded.manager_key`, c.Host, c.Port, enc)
	return err
}

// ContainerManagerKey returns the decrypted private management key of a
// container, or "" when there is none.
func (st *Store) ContainerManagerKey(c *Container) (string, error) {
	var enc string
	err := st.db.QueryRow(`SELECT manager_key FROM client_containers WHERE host = ? AND port = ?`, c.Host, c.Port).Scan(&enc)
	if err = notFound(err); err == ErrNotFound {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return st.box.Decrypt(enc)
}

// SetContainerPushResult records the result of pushing the configuration.
func (st *Store) SetContainerPushResult(c *Container, ok bool, output string) error {
	_, err := st.db.Exec(`INSERT INTO client_containers (host, port, pushed_at, push_ok, push_output) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (host, port) DO UPDATE SET pushed_at = excluded.pushed_at, push_ok = excluded.push_ok,
		push_output = excluded.push_output`, c.Host, c.Port, now(), ok, output)
	return err
}
