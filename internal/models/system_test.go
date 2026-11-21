package models

import "testing"

func TestRepoLocation(t *testing.T) {
	cases := []struct {
		s    System
		want string
	}{
		{System{Location: LocationLocal, RepoPath: "/repos/a"}, "/repos/a"},
		{System{Location: LocationRemote, Host: "h", Port: 22, SSHUser: "borg", RepoPath: "/srv/a"}, "ssh://borg@h:22/srv/a"},
		{System{Location: LocationRemote, Host: "h", Port: 2222, SSHUser: "borg", RepoPath: "repo"}, "ssh://borg@h:2222/./repo"},
		{System{Location: LocationRemote, Host: "h", SSHUser: "borg", RepoPath: "~/repo"}, "ssh://borg@h:22/~/repo"},
		{System{Location: LocationRemote, Host: "::1", Port: 22, SSHUser: "borg", RepoPath: "/r"}, "ssh://borg@[::1]:22/r"},
	}
	for _, c := range cases {
		if got := c.s.RepoLocation(); got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}

func TestArchivePrefixFor(t *testing.T) {
	for in, want := range map[string]string{"web-01": "web-01", "Mail server.example": "Mail_server_example", "...": "client"} {
		if got := ArchivePrefixFor(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
