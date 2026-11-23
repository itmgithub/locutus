// Package client sets up the Locutus client container: an SSH server that
// serves borg repositories to Locutus (the repository role), lets Locutus
// back up and restore the volumes mounted into the container (the client
// role), or both.
//
// The container itself only knows the management key of Locutus
// (LOCUTUS_MANAGER_KEY). Locutus pushes the rest of the configuration over
// SSH with that key: the repositories with their keys and the client role.
// The pushed configuration is kept in the configuration volume, so the
// container also starts without Locutus.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Environment variables of the container.
const (
	envManagerKey  = "LOCUTUS_MANAGER_KEY"
	envManagerFrom = "LOCUTUS_MANAGER_FROM"
	envUID         = "LOCUTUS_BORG_UID"
	envGID         = "LOCUTUS_BORG_GID"
	envConfigDir   = "LOCUTUS_CONFIG_DIR"
	envReposDir    = "LOCUTUS_REPOS_DIR"
	defaultUID     = 1000
	defaultConfig  = "/config"
	defaultRepos   = "/repos"
	sshUser        = "borg"
	defaultSSHPort = 22
)

// Settings of versions before 1.2 configured the roles in the environment;
// Locutus pushes them now.
var legacySettings = []string{"LOCUTUS_REPOSITORY_", "LOCUTUS_PRUNE_FROM", "LOCUTUS_CLIENT_KEY",
	"LOCUTUS_CLIENT_RESTORE", "LOCUTUS_CLIENT_KNOWN_HOSTS"}

// RepositoryName is the form of a repository directory below /repos.
var RepositoryName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Settings come from the environment of the container (docker-compose.yml).
type Settings struct {
	ManagerKey  string `json:"manager_key"`  // public key with which Locutus pushes the configuration
	ManagerFrom string `json:"manager_from"` // optional: addresses the management key may be used from
	UID         int    `json:"uid"`          // of the SSH user "borg", which owns the repositories
	GID         int    `json:"gid"`
	ConfigDir   string `json:"config_dir"` // persistent: SSH host key, pushed configuration, borg cache
	ReposDir    string `json:"repos_dir"`
}

// Repository is one borg repository served to Locutus.
type Repository struct {
	Name       string // directory name below the repositories directory, e.g. "web-1"
	Path       string // e.g. /repos/web-1
	Key        string // public key of the repository in Locutus (authorized_keys format)
	PruneKey   string // optional public prune key of the repository in Locutus
	AppendOnly bool   // restrict the normal key to borg serve --append-only
	PruneFrom  string // addresses the prune key may be used from, as for from= in authorized_keys
}

// Client is the client role: Locutus may back up and restore the volumes.
type Client struct {
	Key        string // public key of the client in Locutus
	Restore    bool   // allow restores to this container
	KnownHosts string // known_hosts lines of the repository servers
}

// Config is the complete configuration of the container.
type Config struct {
	Settings
	Repositories []Repository
	Client       *Client // nil: no client role
}

// Roles describes the configured roles for the log.
func (c *Config) Roles() string {
	var roles []string
	if len(c.Repositories) > 0 {
		roles = append(roles, "repository server")
	}
	if c.Client != nil {
		roles = append(roles, "client")
	}
	if len(roles) == 0 {
		return "none yet: waiting for the configuration from Locutus"
	}
	return strings.Join(roles, " and ")
}

// LoadSettings reads the settings from environment variables (os.Environ
// format) and checks them.
func LoadSettings(environ []string) (*Settings, error) {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = strings.TrimSpace(v)
		}
	}
	s := &Settings{ConfigDir: defaultConfig, ReposDir: defaultRepos}
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	var legacy []string
	for k := range env {
		for _, l := range legacySettings {
			if strings.HasPrefix(k, l) {
				legacy = append(legacy, k)
			}
		}
	}
	if len(legacy) > 0 {
		sort.Strings(legacy)
		fail("%s: these settings are no longer used. Locutus now pushes the repositories and the client role; "+
			"remove them from docker-compose.yml and set %s instead", strings.Join(legacy, ", "), envManagerKey)
	}

	var err error
	if v := env[envManagerKey]; v == "" {
		fail("%s is not set: generate the management key of this container in Locutus (Systems, location Locutus client) and put its public key here", envManagerKey)
	} else if s.ManagerKey, err = publicKey(envManagerKey, v); err != nil {
		errs = append(errs, err)
	}
	if s.ManagerFrom, err = fromList(envManagerFrom, env[envManagerFrom]); err != nil {
		errs = append(errs, err)
	}
	if v := env[envConfigDir]; v != "" {
		s.ConfigDir = v
	}
	if v := env[envReposDir]; v != "" {
		s.ReposDir = strings.TrimSuffix(v, "/")
	}
	for _, d := range []string{s.ConfigDir, s.ReposDir} {
		if !strings.HasPrefix(d, "/") || strings.ContainsAny(d, " \t\"'\\,\n") {
			fail("invalid directory %q", d)
		}
	}
	if s.UID, err = idFrom(env, envUID, defaultUID); err != nil {
		fail("%v", err)
	}
	if s.GID, err = idFrom(env, envGID, s.UID); err != nil {
		fail("%v", err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return s, nil
}

// Pushed is the configuration that Locutus pushes to the container, as JSON
// on the standard input of "locutus-client apply".
type Pushed struct {
	Repositories []PushedRepository `json:"repositories"`
	Client       *PushedClient      `json:"client,omitempty"`
}

// PushedRepository is a repository in the pushed configuration. Name is the
// directory below /repos. With a prune key the normal key is append-only.
type PushedRepository struct {
	Name     string `json:"name"`
	Key      string `json:"key"`
	PruneKey string `json:"prune_key,omitempty"`
}

// PushedClient is the client role in the pushed configuration.
type PushedClient struct {
	Key        string `json:"key"`
	Restore    bool   `json:"restore"`
	KnownHosts string `json:"known_hosts,omitempty"`
}

// ApplyResult is what "locutus-client apply" reports back to Locutus, as
// JSON on its standard output.
type ApplyResult struct {
	Version            string   `json:"version"`
	Build              string   `json:"build"`
	From               string   `json:"from"`
	Roles              string   `json:"roles"`
	PruneFrom          string   `json:"prune_from,omitempty"`
	HostKeyFingerprint string   `json:"host_key_fingerprint"`
	OnDisk             []string `json:"on_disk"` // directories in the repositories directory
	Warnings           []string `json:"warnings,omitempty"`
}

// State is the pushed configuration as kept in the configuration volume,
// together with the address Locutus pushed it from.
type State struct {
	Pushed Pushed `json:"pushed"`
	From   string `json:"from"`
}

// ParsePushed decodes a pushed configuration.
func ParsePushed(data []byte) (*Pushed, error) {
	var p Pushed
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("the configuration from Locutus is not valid JSON: %v", err)
	}
	return &p, nil
}

// Build checks a pushed configuration and combines it with the settings.
// from is the address Locutus pushed from: the prune keys may be used from
// there, unless LOCUTUS_MANAGER_FROM is set, which is used instead.
func Build(s *Settings, p *Pushed, from string) (*Config, error) {
	c := &Config{Settings: *s}
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	pruneFrom := s.ManagerFrom
	if pruneFrom == "" && from != "" {
		if net.ParseIP(from) == nil {
			fail("%q is not an IP address", from)
		}
		pruneFrom = from
	}
	seen := map[string]bool{}
	var err error
	for _, pr := range p.Repositories {
		if !RepositoryName.MatchString(pr.Name) {
			fail("repository %q: the name must consist of lower case letters, digits, '.', '_' and '-'", pr.Name)
			continue
		}
		if seen[pr.Name] {
			fail("repository %s is given twice", pr.Name)
			continue
		}
		seen[pr.Name] = true
		r := Repository{Name: pr.Name, Path: c.ReposDir + "/" + pr.Name}
		if r.Key, err = publicKey("the key of repository "+pr.Name, pr.Key); err != nil {
			errs = append(errs, err)
		}
		if pr.PruneKey != "" {
			if r.PruneKey, err = publicKey("the prune key of repository "+pr.Name, pr.PruneKey); err != nil {
				errs = append(errs, err)
			}
			// Append-only is the safe choice; it only works together with
			// a prune key, which may only be used from Locutus.
			r.AppendOnly, r.PruneFrom = true, pruneFrom
			if pruneFrom == "" {
				fail("repository %s has a prune key, but it is unknown where Locutus connects from", pr.Name)
			}
		}
		c.Repositories = append(c.Repositories, r)
	}
	sort.Slice(c.Repositories, func(i, j int) bool { return c.Repositories[i].Name < c.Repositories[j].Name })

	if pc := p.Client; pc != nil {
		cl := &Client{Restore: pc.Restore}
		if cl.Key, err = publicKey("the client key", pc.Key); err != nil {
			errs = append(errs, err)
		}
		if cl.KnownHosts, err = knownHosts(pc.KnownHosts); err != nil {
			errs = append(errs, err)
		}
		c.Client = cl
	}

	// sshd uses the first line with a matching key, so one key can have
	// only one purpose.
	used := map[string]string{}
	check := func(what, key string) {
		if key == "" {
			return
		}
		k := strings.Join(strings.Fields(key)[:2], " ") // without the comment
		if other, ok := used[k]; ok {
			fail("%s is the same key as %s: generate a separate key pair for each in Locutus", what, other)
		}
		used[k] = what
	}
	check("the management key", c.ManagerKey)
	for _, r := range c.Repositories {
		check("the key of repository "+r.Name, r.Key)
		check("the prune key of repository "+r.Name, r.PruneKey)
	}
	if c.Client != nil {
		check("the client key", c.Client.Key)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func idFrom(env map[string]string, key string, def int) (int, error) {
	v := env[key]
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65534 {
		return 0, fmt.Errorf("%s: %q is not a valid id (1-65534)", key, v)
	}
	return n, nil
}

// publicKey checks a public key in authorized_keys format and returns it
// normalised. Options are refused: the container sets them itself.
func publicKey(name, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("%s is missing", name)
	}
	if strings.ContainsAny(v, "\r\n") {
		return "", fmt.Errorf("%s: only one key, on one line", name)
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(v))
	if err != nil {
		return "", fmt.Errorf("%s: not a valid public key: %v", name, err)
	}
	if len(options) > 0 || len(rest) > 0 {
		return "", fmt.Errorf("%s: give only the public key, without options such as command= or restrict", name)
	}
	out := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if comment = strings.Join(strings.Fields(comment), "-"); comment != "" {
		out += " " + comment
	}
	return out, nil
}

// Fingerprint returns the SHA256 fingerprint of a public key in
// authorized_keys format, or "" when it is not one.
func Fingerprint(authorizedKey string) string {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(key)
}

// fromList checks a comma separated list of IP addresses or networks (as
// in from= of authorized_keys) and returns it normalised.
func fromList(name, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		_, _, cidrErr := net.ParseCIDR(p)
		if net.ParseIP(p) == nil && cidrErr != nil {
			return "", fmt.Errorf("%s: %q is not an IP address or network", name, p)
		}
		out = append(out, p)
	}
	return strings.Join(out, ","), nil
}

// knownHosts checks the known_hosts lines of the repository servers.
func knownHosts(v string) (string, error) {
	var lines []string
	for _, l := range strings.Split(v, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if _, _, _, _, _, err := ssh.ParseKnownHosts([]byte(l)); err != nil {
			return "", fmt.Errorf("the host key of a repository server, %q, is not a valid known_hosts line: %v", l, err)
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}
