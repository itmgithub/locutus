// locutus-client is the entrypoint of the Locutus client container. It
// reads its settings from the environment (docker-compose.yml): only the
// management key of Locutus and a few optional settings. Locutus pushes the
// rest of the configuration over SSH with that key ("locutus-client apply"):
//
//   - repository server: borg repositories in /repos that Locutus (and the
//     clients that Locutus backs up) reach with borg serve;
//   - client: Locutus backs up and restores the volumes mounted into the
//     container, as for any Linux client with sudo and the wrapper.
//
// The pushed configuration is kept in /config, so the container starts with
// it without Locutus.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"syscall"

	"itmon.nl/locutus/internal/client"
	"itmon.nl/locutus/internal/version"
)

// maxPushed limits the size of a pushed configuration.
const maxPushed = 1 << 20

func main() {
	if len(os.Args) == 2 && os.Args[1] == "apply" {
		os.Exit(apply())
	}
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: locutus-client [apply]")
		os.Exit(2)
	}
	start()
}

// start sets up the container with the configuration pushed last and runs
// the SSH server.
func start() {
	log.SetFlags(log.LstdFlags)
	log.Printf("Locutus client %s (Build: %s) starting", version.Version, version.Build)

	set, err := client.LoadSettings(os.Environ())
	if err != nil {
		log.Fatalf("configuration:\n%v", err)
	}
	sys := client.Container()
	if err := sys.SaveSettings(set); err != nil {
		log.Fatalf("setup: %v", err)
	}
	cfg := &client.Config{Settings: *set}
	st, err := sys.LoadState(set)
	if err == nil {
		var pushed *client.Config
		if pushed, err = client.Build(set, &st.Pushed, st.From); err == nil {
			cfg = pushed
		}
	}
	if err != nil {
		log.Printf("warning: the configuration that Locutus pushed earlier cannot be used, push it again: %v", err)
	}
	from := "anywhere"
	if set.ManagerFrom != "" {
		from = set.ManagerFrom
	}
	log.Printf("management key: %s (from %s)", client.Fingerprint(set.ManagerKey), from)
	logConfig(cfg)

	res, err := sys.Apply(cfg)
	if err != nil {
		log.Fatalf("setup: %v", err)
	}
	if res.NewHostKey {
		log.Printf("new SSH host key created in %s/ssh", set.ConfigDir)
	}
	log.Printf("SSH host key: %s (%s)", res.HostKey, res.HostKeyFingerprint)
	if len(cfg.Repositories) == 0 && cfg.Client == nil {
		log.Printf("waiting for the configuration from Locutus: compare the host key above with the one Locutus shows, then push the configuration")
	}

	path, args, err := client.SSHDCommand()
	if err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("listening on port 22 as user borg")
	if err := syscall.Exec(path, args, []string{"PATH=" + os.Getenv("PATH"), "TZ=" + os.Getenv("TZ")}); err != nil {
		log.Fatalf("start sshd: %v", err)
	}
}

func logConfig(cfg *client.Config) {
	log.Printf("roles: %s", cfg.Roles())
	for _, r := range cfg.Repositories {
		mode := "full access"
		if r.AppendOnly {
			mode = "append-only, prune key from " + r.PruneFrom
		}
		log.Printf("repository %s: %s (%s)", r.Name, r.Path, mode)
	}
	if c := cfg.Client; c != nil {
		restore := "allowed"
		if !c.Restore {
			restore = "refused"
		}
		log.Printf("client: backups through sudo and the wrapper, restores %s", restore)
		if c.KnownHosts == "" {
			log.Printf("warning: the host key of the repository server is unknown: backups cannot reach it")
		}
	}
}

// apply is run by Locutus over SSH with the management key, as root through
// sudo: it checks and applies the configuration on the standard input and
// reports the result as JSON on the standard output.
func apply() int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "locutus-client: "+format+"\n", args...)
		return 1
	}
	if os.Geteuid() != 0 {
		return fail("apply must run as root")
	}
	// One configuration at a time.
	lock, err := os.OpenFile("/run/locutus-client.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fail("%v", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fail("%v", err)
	}

	sys := client.Container()
	set, err := sys.LoadSettings()
	if err != nil {
		return fail("settings of the container: %v", err)
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, maxPushed+1))
	if err != nil {
		return fail("%v", err)
	}
	if len(data) > maxPushed {
		return fail("the configuration is too large")
	}
	pushed, err := client.ParsePushed(data)
	if err != nil {
		return fail("%v", err)
	}
	from, _, _ := strings.Cut(os.Getenv("SSH_CLIENT"), " ")
	cfg, err := client.Build(set, pushed, from)
	if err != nil {
		return fail("the configuration is refused:\n%v", err)
	}
	res, err := sys.Apply(cfg)
	if err != nil {
		return fail("setup: %v", err)
	}
	if err := sys.SaveState(set, &client.State{Pushed: *pushed, From: from}); err != nil {
		return fail("save the configuration: %v", err)
	}

	out := client.ApplyResult{Version: version.Version, Build: version.Build, From: from, Roles: cfg.Roles(),
		HostKeyFingerprint: res.HostKeyFingerprint}
	for _, r := range cfg.Repositories {
		if r.PruneFrom != "" {
			out.PruneFrom = r.PruneFrom
		}
	}
	if out.OnDisk, err = sys.OnDisk(set); err != nil {
		out.Warnings = append(out.Warnings, fmt.Sprintf("the repositories directory cannot be read: %v", err))
	}
	for _, d := range out.OnDisk {
		if !slices.ContainsFunc(cfg.Repositories, func(r client.Repository) bool { return r.Name == d }) {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s/%s is not a repository in Locutus", set.ReposDir, d))
		}
	}

	// Also in the log of the container (the output of PID 1).
	if f, err := os.OpenFile("/proc/1/fd/1", os.O_WRONLY|os.O_APPEND, 0); err == nil {
		log.SetOutput(f)
		log.Printf("configuration pushed by Locutus from %s", from)
		logConfig(cfg)
		f.Close()
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return fail("%v", err)
	}
	return 0
}
