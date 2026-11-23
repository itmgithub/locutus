package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testKey(t *testing.T, comment string) string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ssh.NewPublicKey(pub)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " " + comment
}

func testSettings(t *testing.T, env ...string) *Settings {
	t.Helper()
	s, err := LoadSettings(append([]string{"LOCUTUS_MANAGER_KEY=" + testKey(t, "locutus-manager")}, env...))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLoadSettings(t *testing.T) {
	s := testSettings(t, "LOCUTUS_MANAGER_FROM=192.0.2.10, 2001:db8::/64", "LOCUTUS_BORG_UID=1500")
	if s.ManagerFrom != "192.0.2.10,2001:db8::/64" || s.UID != 1500 || s.GID != 1500 || s.ConfigDir != "/config" || s.ReposDir != "/repos" {
		t.Errorf("settings: %+v", s)
	}
	k := testKey(t, "a")
	for name, env := range map[string][]string{
		"no manager key":  {"PATH=/bin"},
		"options in key":  {"LOCUTUS_MANAGER_KEY=command=\"sh\" " + k},
		"two lines":       {"LOCUTUS_MANAGER_KEY=" + k + "\n" + testKey(t, "b")},
		"bad from":        {"LOCUTUS_MANAGER_KEY=" + k, "LOCUTUS_MANAGER_FROM=*"},
		"bad uid":         {"LOCUTUS_MANAGER_KEY=" + k, "LOCUTUS_BORG_UID=0"},
		"legacy repo":     {"LOCUTUS_MANAGER_KEY=" + k, "LOCUTUS_REPOSITORY_NAS_KEY=" + k},
		"legacy client":   {"LOCUTUS_MANAGER_KEY=" + k, "LOCUTUS_CLIENT_KEY=" + k},
		"legacy from":     {"LOCUTUS_MANAGER_KEY=" + k, "LOCUTUS_PRUNE_FROM=192.0.2.1"},
		"bad config path": {"LOCUTUS_MANAGER_KEY=" + k, "LOCUTUS_CONFIG_DIR=config"},
	} {
		if _, err := LoadSettings(env); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBuild(t *testing.T) {
	set := testSettings(t)
	k1, p1, k2, ck, hk := testKey(t, "locutus"), testKey(t, "locutus-prune"), testKey(t, "locutus"), testKey(t, "locutus"), testKey(t, "host")
	c, err := Build(set, &Pushed{
		Repositories: []PushedRepository{{Name: "web-1", Key: k1, PruneKey: p1}, {Name: "nas", Key: k2}},
		Client:       &PushedClient{Key: ck, KnownHosts: "[nas.example.com]:2222 " + strings.Join(strings.Fields(hk)[:2], " ") + "\n"},
	}, "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Repositories) != 2 || c.Repositories[0].Name != "nas" || c.Repositories[1].Name != "web-1" {
		t.Fatalf("repositories: %+v", c.Repositories)
	}
	nas, web := c.Repositories[0], c.Repositories[1]
	if nas.AppendOnly || !web.AppendOnly || web.Path != "/repos/web-1" || web.PruneFrom != "192.0.2.10" {
		t.Errorf("unexpected: %+v %+v", nas, web)
	}
	if c.Roles() != "repository server and client" || c.Client.Restore {
		t.Errorf("client: %+v", c.Client)
	}

	cmd := Commands{Sudo: "/usr/bin/sudo", Borg: "/usr/bin/borg", Stat: "/bin/stat", Tar: "/bin/tar"}
	ak := c.AuthorizedKeys(cmd)
	for _, want := range []string{
		`command="/usr/bin/sudo -n /usr/local/bin/locutus-client apply",restrict ` + set.ManagerKey,
		`command="borg serve --restrict-to-repository /repos/nas",restrict ` + k2,
		`command="borg serve --append-only --restrict-to-repository /repos/web-1",restrict ` + k1,
		`from="192.0.2.10",command="borg serve --restrict-to-repository /repos/web-1",restrict ` + p1,
		`command="/usr/local/bin/locutus-borg-wrapper --no-restore",restrict,agent-forwarding ` + ck,
	} {
		if !strings.Contains(ak, "\n"+want+"\n") {
			t.Errorf("authorized_keys lacks\n%s\nin\n%s", want, ak)
		}
	}
	sudo := c.Sudoers(cmd)
	for _, want := range []string{"borg ALL=(root) NOPASSWD: /usr/local/bin/locutus-client apply\n",
		"Defaults!/usr/local/bin/locutus-client env_keep += \"SSH_CLIENT\"\n", "borg ALL=(root) NOPASSWD: /usr/bin/borg\n"} {
		if !strings.Contains(sudo, want) {
			t.Errorf("sudoers lacks %q:\n%s", want, sudo)
		}
	}
	if strings.Contains(sudo, "-x -p") {
		t.Errorf("sudoers allows restores:\n%s", sudo)
	}

	// LOCUTUS_MANAGER_FROM restricts the management key and is used for
	// the prune keys instead of the address Locutus pushed from.
	set.ManagerFrom = "198.51.100.0/24"
	c, err = Build(set, &Pushed{Repositories: []PushedRepository{{Name: "a", Key: k1, PruneKey: p1}}}, "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if c.Repositories[0].PruneFrom != "198.51.100.0/24" ||
		!strings.Contains(c.AuthorizedKeys(cmd), `from="198.51.100.0/24",command="/usr/bin/sudo -n`) {
		t.Errorf("manager from:\n%s", c.AuthorizedKeys(cmd))
	}

	// Nothing pushed yet: only the management key.
	c, err = Build(testSettings(t), &Pushed{}, "")
	if err != nil || len(c.Repositories) != 0 || c.Client != nil || !strings.HasPrefix(c.Roles(), "none") {
		t.Errorf("empty: %+v %v", c, err)
	}
}

func TestBuildRefuses(t *testing.T) {
	set := testSettings(t)
	k, other := testKey(t, "a"), testKey(t, "b")
	for name, p := range map[string]Pushed{
		"bad name":           {Repositories: []PushedRepository{{Name: "../etc", Key: k}}},
		"upper case name":    {Repositories: []PushedRepository{{Name: "NAS", Key: k}}},
		"same name twice":    {Repositories: []PushedRepository{{Name: "a", Key: k}, {Name: "a", Key: other}}},
		"no key":             {Repositories: []PushedRepository{{Name: "a"}}},
		"options in key":     {Repositories: []PushedRepository{{Name: "a", Key: "command=\"sh\" " + k}}},
		"same key twice":     {Repositories: []PushedRepository{{Name: "a", Key: k}}, Client: &PushedClient{Key: k}},
		"manager key reused": {Client: &PushedClient{Key: set.ManagerKey}},
		"bad known hosts":    {Client: &PushedClient{Key: k, KnownHosts: "nas.example.com"}},
		"prune without from": {Repositories: []PushedRepository{{Name: "a", Key: k, PruneKey: other}}},
	} {
		if _, err := Build(set, &p, ""); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestApply(t *testing.T) {
	root := t.TempDir()
	for f, content := range map[string]string{
		"etc/passwd": "root:x:0:0:root:/root:/bin/sh\n",
		"etc/group":  "root:x:0:\n",
		"etc/shadow": "root:*::0:::::\n",
	} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(root, f), []byte(content), 0o644)
	}
	sys := &System{Root: root, Chown: func(string, int, int) error { return nil },
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil }}
	set := testSettings(t)
	if err := sys.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	if got, err := sys.LoadSettings(); err != nil || *got != *set {
		t.Errorf("settings: %+v %v", got, err)
	}

	// The first start, without a pushed configuration.
	st, err := sys.LoadState(set)
	if err != nil || st.Pushed.Client != nil || len(st.Pushed.Repositories) != 0 {
		t.Fatalf("state: %+v %v", st, err)
	}
	c, err := Build(set, &st.Pushed, st.From)
	if err != nil {
		t.Fatal(err)
	}
	first, err := sys.Apply(c)
	if err != nil {
		t.Fatal(err)
	}
	ak, _ := os.ReadFile(filepath.Join(root, "etc/ssh/authorized_keys/borg"))
	if !strings.Contains(string(ak), "locutus-client apply") || strings.Contains(string(ak), "borg serve") {
		t.Errorf("authorized_keys before the push:\n%s", ak)
	}

	// A push.
	pushed := Pushed{Repositories: []PushedRepository{{Name: "a", Key: testKey(t, "a")}}, Client: &PushedClient{Key: testKey(t, "c"), Restore: true}}
	if c, err = Build(set, &pushed, "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	second, err := sys.Apply(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := sys.SaveState(set, &State{Pushed: pushed, From: "192.0.2.10"}); err != nil {
		t.Fatal(err)
	}
	if st, err = sys.LoadState(set); err != nil || st.From != "192.0.2.10" || len(st.Pushed.Repositories) != 1 {
		t.Errorf("state after push: %+v %v", st, err)
	}
	if !first.NewHostKey || second.NewHostKey || first.HostKeyFingerprint != second.HostKeyFingerprint {
		t.Errorf("host key not kept: %+v %+v", first, second)
	}
	passwd, _ := os.ReadFile(filepath.Join(root, "etc/passwd"))
	if strings.Count(string(passwd), "\nborg:") != 1 || !strings.Contains(string(passwd), "\nborg:x:1000:1000:") {
		t.Errorf("passwd:\n%s", passwd)
	}
	if fi, err := os.Stat(filepath.Join(root, "etc/sudoers.d/locutus")); err != nil || fi.Mode().Perm() != 0o440 {
		t.Errorf("sudoers: %v %v", fi, err)
	}
	if l, _ := os.Readlink(filepath.Join(root, "root/.cache/borg")); l != "/config/borg/cache" {
		t.Errorf("cache link %q", l)
	}
	os.MkdirAll(filepath.Join(root, "repos/old"), 0o700)
	if dirs, err := sys.OnDisk(set); err != nil || strings.Join(dirs, ",") != "old" {
		t.Errorf("on disk: %v %v", dirs, err)
	}

	// Without the client role, only the rule for the management key is left.
	c.Client = nil
	if _, err := sys.Apply(c); err != nil {
		t.Fatal(err)
	}
	sudo, _ := os.ReadFile(filepath.Join(root, "etc/sudoers.d/locutus"))
	if !strings.Contains(string(sudo), "locutus-client apply") || strings.Contains(string(sudo), "/usr/bin/borg") {
		t.Errorf("sudoers without client:\n%s", sudo)
	}

	// An id that belongs to another user is refused.
	os.WriteFile(filepath.Join(root, "etc/passwd"), append(passwd, []byte("app:x:1000:1000::/:/bin/sh\n")...), 0o644)
	if _, err := sys.Apply(c); err == nil || !strings.Contains(err.Error(), "LOCUTUS_BORG_UID") {
		t.Errorf("uid conflict: %v", err)
	}
}
