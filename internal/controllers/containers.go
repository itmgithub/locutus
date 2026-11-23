package controllers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"itmon.nl/locutus/internal/client"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/remote"
	"itmon.nl/locutus/internal/version"
)

// pushTimeout limits pushing the configuration to one container.
const pushTimeout = time.Minute

// errNoManagerKey: the container has no management key yet, so Locutus
// cannot push its configuration.
var errNoManagerKey = errors.New("no management key")

// pushLocks makes sure only one push per container runs at a time, so the
// last change always wins.
var pushLocks sync.Map

// containerConfig builds the configuration of a container from its systems
// in Locutus. The warnings are about systems that cannot be included yet.
func (a *App) containerConfig(c *models.Container, lang string) (*client.Pushed, []string, error) {
	systems, err := a.store.ContainerSystems(c)
	if err != nil {
		return nil, nil, err
	}
	tr := func(key string, args ...any) string { return a.tr.T(lang, key, args...) }
	p := &client.Pushed{Repositories: []client.PushedRepository{}}
	var warnings []string
	for _, s := range systems {
		sec, err := a.store.SystemSecrets(s.ID)
		if err != nil {
			return nil, nil, err
		}
		key, _ := publicKeyOf(sec.SSHKey, "locutus-"+s.Name)
		if key == "" {
			warnings = append(warnings, tr("container.warn_no_key", s.Name))
			continue
		}
		if s.IsRepository() {
			prune, _ := publicKeyOf(sec.PruneSSHKey, "locutus-prune-"+s.Name)
			p.Repositories = append(p.Repositories, client.PushedRepository{Name: s.ContainerName(), Key: key, PruneKey: prune})
			continue
		}
		if p.Client != nil {
			warnings = append(warnings, tr("container.warn_second_client", s.Name))
			continue
		}
		p.Client = &client.PushedClient{Key: key, Restore: s.AllowRestore}
		repo, err := a.store.GetSystem(s.RepositoryID)
		if err != nil {
			return nil, nil, err
		}
		keys, err := a.borg.HostKeys().Known(repo.SSHAddress())
		if err != nil {
			return nil, nil, err
		}
		for _, k := range keys {
			p.Client.KnownHosts += remote.KnownHostsLine(repo.SSHAddress(), k) + "\n"
		}
		if len(keys) == 0 {
			warnings = append(warnings, tr("container.warn_repo_host_key", repo.Name))
		}
	}
	return p, warnings, nil
}

// pushContainer pushes the configuration to a container and records the
// result (with the error, if any). It returns the output for the admin;
// the error is not part of it.
func (a *App) pushContainer(ctx context.Context, c *models.Container, lang string) (string, error) {
	mu, _ := pushLocks.LoadOrStore(c.Key(), &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	out, err := a.doPush(ctx, c, lang)
	if errors.Is(err, errNoManagerKey) {
		return "", err
	}
	recorded := out
	if err != nil {
		if strings.Contains(err.Error(), "key mismatch") {
			err = fmt.Errorf("%v\n%s", err, a.tr.T(lang, "container.host_key_mismatch"))
		}
		recorded = strings.TrimSpace(out + "\n" + err.Error())
	}
	if serr := a.store.SetContainerPushResult(c, err == nil, recorded); serr != nil {
		log.Printf("record push to %s: %v", c.SSHAddress(), serr)
	}
	return out, err
}

func (a *App) doPush(ctx context.Context, c *models.Container, lang string) (string, error) {
	tr := func(key string, args ...any) string { return a.tr.T(lang, key, args...) }
	managerKey, err := a.store.ContainerManagerKey(c)
	if err != nil {
		return "", err
	}
	if managerKey == "" {
		return "", errNoManagerKey
	}
	p, warnings, err := a.containerConfig(c, lang)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	t := remote.Target{Address: c.SSHAddress(), User: models.ContainerSSHUser, PrivateKey: managerKey}
	rc, err := remote.RunIO(ctx, a.borg.HostKeys(), t, "apply", bytes.NewReader(data), &stdout, &stderr, nil)
	if err == nil && rc != 0 {
		err = fmt.Errorf("%s", strings.TrimSpace(stderr.String()))
		if stderr.Len() == 0 {
			err = fmt.Errorf("exit code %d", rc)
		}
	}
	if err != nil {
		return strings.Join(warnings, "\n"), err
	}
	var res client.ApplyResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return "", fmt.Errorf("unexpected answer from the container: %q", strings.TrimSpace(stdout.String()))
	}

	var b strings.Builder
	fmt.Fprintln(&b, tr("container.pushed", models.ContainerSSHUser+"@"+c.SSHAddress(), res.Version, res.Build))
	fmt.Fprintln(&b, tr("container.roles", res.Roles))
	for _, r := range p.Repositories {
		mode := tr("container.full_access")
		if r.PruneKey != "" {
			mode = tr("container.append_only", res.PruneFrom)
		}
		state := tr("container.not_initialised")
		for _, d := range res.OnDisk {
			if d == r.Name {
				state = tr("container.on_disk")
			}
		}
		fmt.Fprintln(&b, tr("container.repository", models.ContainerReposDir+r.Name, mode, state))
	}
	if p.Client != nil {
		restore := tr("container.restore_refused")
		if p.Client.Restore {
			restore = tr("container.restore_allowed")
		}
		fmt.Fprintln(&b, tr("container.client", restore))
	}
	fmt.Fprintln(&b, tr("container.host_key", res.HostKeyFingerprint))
	if res.Version != version.Version {
		warnings = append(warnings, tr("container.warn_version", res.Version, version.Version))
	}
	warnings = append(warnings, res.Warnings...)
	for _, w := range warnings {
		fmt.Fprintln(&b, tr("container.warning", w))
	}
	return strings.TrimSpace(b.String()), nil
}

// pushAffected pushes, in the background, the configuration of every
// container that the change of these systems affects: the container of a
// system itself, and the containers of the clients that back up to a
// repository (they need its address and host key).
func (a *App) pushAffected(r *http.Request, systems ...*models.System) {
	targets := map[string]*models.Container{}
	add := func(s *models.System) {
		if s.IsLocutusClient() {
			c := &models.Container{Host: s.Host, Port: s.Port}
			targets[c.Key()] = c
		}
	}
	for _, s := range systems {
		if s == nil {
			continue
		}
		add(s)
		if s.IsRepository() && s.ID != 0 {
			clients, err := a.store.ListRepositoryClients(s.ID)
			if err != nil {
				log.Printf("push: %v", err)
			}
			for _, c := range clients {
				add(c)
			}
		}
	}
	lang := a.tr.Language(r)
	for _, c := range targets {
		go func() {
			if _, err := a.pushContainer(context.Background(), c, lang); err != nil && !errors.Is(err, errNoManagerKey) {
				log.Printf("push the configuration to %s: %v", c.SSHAddress(), err)
			}
		}()
	}
}

// containerOfPath returns the system of the path and its container; it
// renders an error when the system is not in a Locutus client container.
func (a *App) containerOfPath(w http.ResponseWriter, r *http.Request) (*models.System, *models.Container) {
	s := a.systemFromPath(w, r)
	if s == nil {
		return nil, nil
	}
	c, err := a.store.ContainerOf(s)
	if err != nil {
		a.serverError(w, r, err)
		return nil, nil
	}
	if c == nil {
		a.errorPage(w, r, http.StatusBadRequest, "error.not_found")
		return nil, nil
	}
	return s, c
}

// adminContainerGenerateKey creates a new management key for the container
// of a system. The admin puts its public key in LOCUTUS_MANAGER_KEY.
func (a *App) adminContainerGenerateKey(w http.ResponseWriter, r *http.Request) {
	s, c := a.containerOfPath(w, r)
	if s == nil {
		return
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	block, err := ssh.MarshalPrivateKey(priv, "locutus-manager "+c.SSHAddress())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if err := a.store.SetContainerManagerKey(c, string(pem.EncodeToMemory(block))); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", "container.key_generated")
	http.Redirect(w, r, "/admin/systems/"+itoa(s.ID)+"/edit#container", http.StatusSeeOther)
}

// adminContainerPush pushes the configuration now and shows the result.
func (a *App) adminContainerPush(w http.ResponseWriter, r *http.Request) {
	s, c := a.containerOfPath(w, r)
	if s == nil {
		return
	}
	out, err := a.pushContainer(r.Context(), c, a.tr.Language(r))
	if errors.Is(err, errNoManagerKey) {
		out, err = "", errors.New(a.t(r, "container.no_manager_key"))
	}
	a.renderContainerResult(w, r, s, out, err, nil)
}

// adminContainerHostKey compares the host key that the container presents
// now with the one Locutus knows. When they differ, the admin can accept
// the new one after comparing it with the log of the container.
func (a *App) adminContainerHostKey(w http.ResponseWriter, r *http.Request) {
	s, c := a.containerOfPath(w, r)
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	presented, err := remote.FetchHostKey(ctx, c.SSHAddress())
	if err != nil {
		a.renderContainerResult(w, r, s, "", err, nil)
		return
	}
	known, err := a.borg.HostKeys().Known(c.SSHAddress())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	fp := ssh.FingerprintSHA256(presented)
	var knownFPs []string
	for _, k := range known {
		if ssh.FingerprintSHA256(k) == fp {
			a.renderContainerResult(w, r, s, a.t(r, "container.host_key_same", fp), nil, nil)
			return
		}
		knownFPs = append(knownFPs, ssh.FingerprintSHA256(k))
	}
	out := a.t(r, "container.host_key_presented", fp)
	if len(knownFPs) > 0 {
		out += "\n" + a.t(r, "container.host_key_known", strings.Join(knownFPs, ", "))
	} else {
		out += "\n" + a.t(r, "container.host_key_unknown")
	}
	a.renderContainerResult(w, r, s, out, nil, &hostKeyOffer{Fingerprint: fp})
}

// adminContainerAcceptHostKey records the host key that the admin has
// checked and pushes the configuration again, also to the containers whose
// clients back up to repositories in this container.
func (a *App) adminContainerAcceptHostKey(w http.ResponseWriter, r *http.Request) {
	s, c := a.containerOfPath(w, r)
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	presented, err := remote.FetchHostKey(ctx, c.SSHAddress())
	if err != nil {
		a.renderContainerResult(w, r, s, "", err, nil)
		return
	}
	// Only the key the admin has seen: not one that changed in between.
	if ssh.FingerprintSHA256(presented) != r.FormValue("fingerprint") {
		a.renderContainerResult(w, r, s, "", errors.New(a.t(r, "container.host_key_changed_again")), nil)
		return
	}
	if err := a.borg.HostKeys().Replace(c.SSHAddress(), presented); err != nil {
		a.serverError(w, r, err)
		return
	}
	systems, err := a.store.ContainerSystems(c)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	var others []*models.System
	for _, o := range systems {
		if o.IsRepository() {
			others = append(others, o)
		}
	}
	a.pushAffected(r, others...)
	out, err := a.pushContainer(r.Context(), c, a.tr.Language(r))
	if errors.Is(err, errNoManagerKey) {
		out, err = a.t(r, "container.host_key_accepted"), nil
	}
	a.renderContainerResult(w, r, s, out, err, nil)
}

// hostKeyOffer is a new host key that the admin may accept.
type hostKeyOffer struct {
	Fingerprint string
}

func (a *App) renderContainerResult(w http.ResponseWriter, r *http.Request, s *models.System, out string, err error, offer *hostKeyOffer) {
	data, derr := a.systemFormData(s)
	if derr != nil {
		a.serverError(w, r, derr)
		return
	}
	data.Tested, data.TestOK, data.TestOutput, data.HostKeyOffer = true, err == nil, out, offer
	if err != nil {
		data.TestOutput = strings.TrimSpace(out + "\n" + err.Error())
	}
	a.renderSystemForm(w, r, http.StatusOK, data, nil)
}
