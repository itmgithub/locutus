package borg

import (
	"reflect"
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/etc":           "/etc",
		"ssh://a@b:22/x": "ssh://a@b:22/x",
		"/home/my files": "'/home/my files'",
		"*.tmp":          "'*.tmp'",
		"it's":           `'it'\''s'`,
		"":               "''",
		"--exclude=/a b": "'--exclude=/a b'",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestV1Args(t *testing.T) {
	d := v1{}
	got := d.CreateArgs("/repo", "web.2026-01-01T00:00:00", CreateOptions{
		Paths: []string{"/etc", "/home"}, Excludes: []string{"*.tmp"}, Compression: "lz4", OneFileSystem: true})
	want := []string{"create", "--info", "--stats", "--show-rc", "--lock-wait", "600", "--compression", "lz4", "--one-file-system",
		"--exclude=*.tmp", "--", "/repo::web.2026-01-01T00:00:00", "/etc", "/home"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("create args\n got %q\nwant %q", got, want)
	}
	got = d.PruneArgs("/repo", "web.*", Retention{Hourly: 24, Daily: 7, Weekly: 4, Monthly: 6})
	want = []string{"prune", "--info", "--list", "--stats", "--show-rc", "--lock-wait", "600", "--keep-hourly", "24", "--keep-daily", "7",
		"--keep-weekly", "4", "--keep-monthly", "6", "--glob-archives", "web.*", "--", "/repo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prune args\n got %q\nwant %q", got, want)
	}
	got = d.ExportTarArgs("/repo", "a", []string{"etc/passwd", "home"})
	want = []string{"export-tar", "--lock-wait", "600", "--", "/repo::a", "-", "pp:etc/passwd", "pp:home"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("export-tar args\n got %q\nwant %q", got, want)
	}
}

func TestParseArchives(t *testing.T) {
	out := []byte(`{"archives":[{"name":"a.1","start":"2026-01-01T10:00:00.000000"},{"name":"a.2","start":"2026-01-02T10:00:00.000000"}]}`)
	list, err := v1{}.ParseArchives(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "a.2" || list[0].Start.IsZero() {
		t.Errorf("unexpected archives %+v", list)
	}
}

func TestTree(t *testing.T) {
	tr := newTree()
	for _, it := range []Item{
		{Path: "home/user/b.txt", Type: "-", Size: 3},
		{Path: "home/user", Type: "d"},
		{Path: "home/user/a", Type: "d"},
		{Path: "etc/hosts", Type: "-"},
	} {
		it.Name = it.Path[len(parentOf(it.Path)):]
		tr.add(it)
	}
	tr.finish()
	root := tr.Children("")
	if len(root) != 2 || root[0].Path != "etc" || root[1].Path != "home" || !root[1].IsDir() {
		t.Fatalf("root = %+v", root)
	}
	kids := tr.Children("/home/user/")
	if len(kids) != 2 || kids[0].Path != "home/user/a" || kids[1].Path != "home/user/b.txt" {
		t.Fatalf("children = %+v", kids)
	}
	if _, ok := tr.Lookup("/etc/hosts"); !ok {
		t.Error("lookup failed")
	}
	if _, ok := tr.Lookup("../etc/hosts"); !ok {
		t.Error("lookup of cleaned path failed")
	}
}

func TestParseItem(t *testing.T) {
	it, err := v1{}.ParseItem([]byte(`{"type":"-","mode":"-rw-r--r--","user":1000,"group":"staff","path":"home/a.txt","size":5,"mtime":"2026-01-01T10:00:00.000000"}`))
	if err != nil {
		t.Fatal(err)
	}
	if it.User != "1000" || it.Group != "staff" || it.Name != "a.txt" || it.Size != 5 {
		t.Errorf("unexpected item %+v", it)
	}
}

func TestRepoForPrune(t *testing.T) {
	r := &Repo{Location: "ssh://borg@h:22/r", Remote: true, SSHKey: "append-only", Passphrase: "p"}
	if got := r.ForPrune(); got != r {
		t.Fatalf("without prune key ForPrune must return the repo itself")
	}
	r.PruneSSHKey = "prune"
	p := r.ForPrune()
	if p.SSHKey != "prune" || p.Passphrase != "p" || p.Location != r.Location {
		t.Fatalf("ForPrune = %+v", p)
	}
	if r.SSHKey != "append-only" {
		t.Fatalf("ForPrune changed the original repo: %+v", r)
	}
}

func TestDeleteArgs(t *testing.T) {
	got := strings.Join(v1{}.DeleteArgs("/repo", "web.2026-01-01T03:00:00"), " ")
	want := "delete --info --stats --show-rc --lock-wait 600 -- /repo::web.2026-01-01T03:00:00"
	if got != want {
		t.Fatalf("DeleteArgs = %q, want %q", got, want)
	}
}
