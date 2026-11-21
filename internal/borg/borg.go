// Package borg wraps the borg command line client. The command lines differ
// per major borg version, so they are produced by a Driver; currently only
// borg 1.x is implemented, a borg 2 driver can be added next to it.
package borg

import (
	"fmt"
	"strings"
	"time"
)

// Repo describes how to reach a borg repository.
type Repo struct {
	Location string // local path or ssh:// URL
	Remote   bool
	SSHKey   string // private key (OpenSSH/PEM) offered through an in-memory ssh-agent
	// PruneSSHKey, when set, replaces SSHKey for prune and compact. It lets the
	// normal key be restricted to "borg serve --append-only". It is only used by
	// Locutus itself and never forwarded to a client.
	PruneSSHKey string
	Passphrase  string
	Version     int // borg major version of the repository
}

// ForPrune returns the repository description to use for prune and compact:
// with the prune key when one is configured.
func (r *Repo) ForPrune() *Repo {
	if r.PruneSSHKey == "" {
		return r
	}
	c := *r
	c.SSHKey = r.PruneSSHKey
	return &c
}

// Client describes the machine on which "borg create" runs.
type Client struct {
	Local   bool   // run inside the Locutus container
	Address string // host:port for remote clients
	User    string
	SSHKey  string // private key used to log in on the client
	// Sudo runs borg (and for restores stat and tar) on the client with
	// "sudo -n", so that the SSH user does not need to be root.
	Sudo bool
}

// Archive is one backup in a repository.
type Archive struct {
	Name  string
	Start time.Time
}

// Item is a file, directory or other entry in an archive.
type Item struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Type       string `json:"type"` // "d" directory, "-" file, "l" symlink, ...
	Mode       string `json:"mode"`
	User       string `json:"user"`
	Group      string `json:"group"`
	Size       int64  `json:"size"`
	Mtime      string `json:"mtime"`
	LinkTarget string `json:"linktarget,omitempty"`
}

// IsDir reports whether the item is a directory.
func (i Item) IsDir() bool { return i.Type == "d" }

// CreateOptions configures a backup.
type CreateOptions struct {
	Paths         []string
	Excludes      []string // borg exclude patterns (default style fm:)
	Compression   string
	OneFileSystem bool
}

// Retention is the prune policy; zero values are not passed to borg.
type Retention struct {
	Hourly, Daily, Weekly, Monthly, Yearly int
}

// Empty reports whether no keep rule is set.
func (r Retention) Empty() bool {
	return r.Hourly+r.Daily+r.Weekly+r.Monthly+r.Yearly == 0
}

// Driver builds the borg command lines (without the binary name) and parses
// the output of one borg major version.
type Driver interface {
	Version() int
	InitArgs(repo, encryption string) []string
	InfoArgs(repo string) []string
	ListArchivesArgs(repo, glob string) []string
	ParseArchives(out []byte) ([]Archive, error)
	ListItemsArgs(repo, archive string) []string
	ParseItem(line []byte) (Item, error)
	ExtractStdoutArgs(repo, archive, path string) []string
	ExportTarArgs(repo, archive string, paths []string) []string
	CreateArgs(repo, archive string, opts CreateOptions) []string
	PruneArgs(repo, glob string, keep Retention) []string
	DeleteArgs(repo, archive string) []string
	CompactArgs(repo string) []string
}

// SupportedVersions are the borg major versions that can be chosen for a
// repository. Add 2 here once the borg 2 driver exists.
var SupportedVersions = []int{1}

// VersionSupported reports whether v can be chosen.
func VersionSupported(v int) bool {
	for _, x := range SupportedVersions {
		if x == v {
			return true
		}
	}
	return false
}

// DriverFor returns the driver for a borg major version.
func DriverFor(version int) (Driver, error) {
	switch version {
	case 0, 1:
		return v1{}, nil
	default:
		return nil, fmt.Errorf("borg version %d is not supported yet", version)
	}
}

// Encryption modes offered when initialising a repository. Keyfile modes are
// not offered: the key would only exist inside the Locutus container, so
// remote clients could not use the repository.
var EncryptionModes = []string{"repokey-blake2", "repokey", "authenticated-blake2", "none"}

// ValidEncryption reports whether mode is offered.
func ValidEncryption(mode string) bool {
	for _, m := range EncryptionModes {
		if m == mode {
			return true
		}
	}
	return false
}

// Compression algorithms offered for backups.
var CompressionModes = []string{"lz4", "zstd", "zstd,3", "zstd,10", "zlib", "lzma", "auto,zstd", "none"}

// ValidCompression reports whether mode is offered.
func ValidCompression(mode string) bool {
	for _, m := range CompressionModes {
		if m == mode {
			return true
		}
	}
	return false
}

// ArchiveName returns the archive name of a new backup of a client.
func ArchiveName(prefix string, t time.Time) string {
	return prefix + "." + t.Format("2006-01-02T15:04:05")
}

// ShellQuote quotes s for a POSIX shell.
func ShellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=,@+", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
