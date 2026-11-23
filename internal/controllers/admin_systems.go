package controllers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"itmon.nl/locutus/internal/borg"
	"itmon.nl/locutus/internal/client"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/scheduler"
	"itmon.nl/locutus/internal/views"
)

type adminSystemsData struct {
	Repositories []*models.System
	Clients      []*models.System
	RepoNames    map[int64]string
}

func (a *App) adminSystems(w http.ResponseWriter, r *http.Request) {
	all, err := a.store.ListSystems("")
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	data := adminSystemsData{RepoNames: map[int64]string{}}
	for _, s := range all {
		if s.IsRepository() {
			data.Repositories = append(data.Repositories, s)
			data.RepoNames[s.ID] = s.Name
		} else {
			data.Clients = append(data.Clients, s)
		}
	}
	a.render(w, r, http.StatusOK, "admin_systems", &views.Page{Title: a.t(r, "admin.systems.title"), Data: data})
}

type systemFormData struct {
	System          *models.System
	Repositories    []*models.System
	Users           []*models.User
	AssignedIDs     []int64
	PublicKey       string
	Fingerprint     string
	PrunePublicKey  string
	PruneFinger     string
	TestOutput      string
	TestOK          bool
	Tested          bool
	EncryptionModes []string
	BorgVersions    []int
	KnownHostsFile  string
	ClientCount     int
	// Locutus client container only.
	Container        *models.Container
	ManagerPublicKey string
	ManagerFinger    string
	HostKeyOffer     *hostKeyOffer
}

// systemFromPath returns the system being edited, or a new one of the kind
// given in the path.
func (a *App) systemFromPath(w http.ResponseWriter, r *http.Request) *models.System {
	if kind := r.PathValue("kind"); kind != "" {
		if kind != models.KindRepository && kind != models.KindClient {
			a.errorPage(w, r, http.StatusNotFound, "error.not_found")
			return nil
		}
		return &models.System{Kind: kind, Location: models.LocationRemote, Port: 22, BorgVersion: 1, AllowRestore: true}
	}
	s, err := a.store.GetSystem(pathID(r, "id"))
	if err != nil {
		a.storeError(w, r, err)
		return nil
	}
	return s
}

// publicKeyOf returns the authorized_keys line (with comment) and the
// fingerprint of a private key.
func publicKeyOf(privateKey, comment string) (string, string) {
	if privateKey == "" {
		return "", ""
	}
	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return "", ""
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	return pub + " " + comment, ssh.FingerprintSHA256(signer.PublicKey())
}

func (a *App) systemFormData(s *models.System) (*systemFormData, error) {
	data := &systemFormData{System: s, EncryptionModes: borg.EncryptionModes, BorgVersions: borg.SupportedVersions,
		KnownHostsFile: a.cfg.KnownHostsFile}
	var err error
	if data.Repositories, err = a.store.ListSystems(models.KindRepository); err != nil {
		return nil, err
	}
	if s.ID == 0 {
		return data, nil
	}
	users, err := a.store.ListUsers()
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.Role != models.RoleAdmin {
			data.Users = append(data.Users, u)
		}
	}
	if data.AssignedIDs, err = a.store.SystemUserIDs(s.ID); err != nil {
		return nil, err
	}
	sec, err := a.store.SystemSecrets(s.ID)
	if err != nil {
		return nil, err
	}
	data.PublicKey, data.Fingerprint = publicKeyOf(sec.SSHKey, "locutus")
	if s.IsRepository() {
		data.PrunePublicKey, data.PruneFinger = publicKeyOf(sec.PruneSSHKey, "locutus-prune")
		if data.ClientCount, err = a.store.CountRepositoryClients(s.ID); err != nil {
			return nil, err
		}
	}
	if data.Container, err = a.store.ContainerOf(s); err != nil || data.Container == nil {
		return data, err
	}
	key, err := a.store.ContainerManagerKey(data.Container)
	if err != nil {
		return nil, err
	}
	data.ManagerPublicKey, data.ManagerFinger = publicKeyOf(key, "locutus-manager")
	return data, nil
}

func (a *App) renderSystemForm(w http.ResponseWriter, r *http.Request, status int, data *systemFormData, errs []string) {
	title := a.t(r, "admin.systems.new_"+data.System.Kind)
	if data.System.ID != 0 {
		title = data.System.Name
	}
	a.render(w, r, status, "admin_system_form", &views.Page{Title: title, Data: data, Errors: errs})
}

func (a *App) adminSystemForm(w http.ResponseWriter, r *http.Request) {
	s := a.systemFromPath(w, r)
	if s == nil {
		return
	}
	data, err := a.systemFormData(s)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.renderSystemForm(w, r, http.StatusOK, data, nil)
}

// adminSystemSubmit creates or updates a repository or client system,
// including its SSH key and (repositories only) passphrase.
func (a *App) adminSystemSubmit(w http.ResponseWriter, r *http.Request) {
	s := a.systemFromPath(w, r)
	if s == nil {
		return
	}
	isNew := s.ID == 0
	before := *s
	s.Name = formText(r, "name")
	s.Description = formText(r, "description")
	s.Location = r.FormValue("location")
	s.Host = formText(r, "host")
	s.SSHUser = formText(r, "ssh_user")
	s.Port, _ = strconv.Atoi(formText(r, "port"))
	if s.Port == 0 {
		s.Port = 22
	}
	var errs []string
	if s.Name == "" {
		errs = append(errs, "error.name_required")
	} else if taken, err := a.store.SystemNameTaken(s.Name, s.ID); err != nil {
		a.serverError(w, r, err)
		return
	} else if taken {
		errs = append(errs, "error.system_name_taken")
	}
	if s.Location != models.LocationLocal && s.Location != models.LocationRemote && s.Location != models.LocationLocutusClient {
		errs = append(errs, "error.location_invalid")
	}
	if s.IsLocutusClient() {
		s.SSHUser = models.ContainerSSHUser
	}
	if s.IsRemote() {
		if s.Host == "" || s.SSHUser == "" || strings.ContainsAny(s.Host+s.SSHUser, " @/'\"") {
			errs = append(errs, "error.remote_required")
		}
		if s.Port < 1 || s.Port > 65535 {
			errs = append(errs, "error.port_invalid")
		}
	}
	if s.IsRepository() {
		s.RepoPath = formText(r, "repo_path")
		s.BorgVersion, _ = strconv.Atoi(r.FormValue("borg_version"))
		if !borg.VersionSupported(s.BorgVersion) {
			errs = append(errs, "error.borg_version")
		}
		if s.RepoPath == "" || strings.Contains(s.RepoPath, "::") || strings.HasPrefix(s.RepoPath, "-") {
			errs = append(errs, "error.repo_path_required")
		} else if !s.IsRemote() && !strings.HasPrefix(s.RepoPath, "/") {
			errs = append(errs, "error.repo_path_absolute")
		} else if s.IsLocutusClient() && !client.RepositoryName.MatchString(s.ContainerName()) {
			errs = append(errs, "error.container_repo_path")
		}
	} else {
		s.UseSudo = s.Location == models.LocationRemote && r.FormValue("use_sudo") == "1"
		s.AllowRestore = true
		if s.IsLocutusClient() {
			// The container sets up sudo itself and refuses restores
			// when they are not allowed here.
			s.UseSudo, s.AllowRestore = true, r.FormValue("allow_restore") == "1"
		}
		s.RepositoryID = formID(r, "repository_id")
		repo, err := a.store.GetSystem(s.RepositoryID)
		switch {
		case err != nil || !repo.IsRepository():
			errs = append(errs, "error.repository_required")
		case s.IsRemote() && !repo.IsRemote():
			errs = append(errs, "error.remote_client_local_repo")
		}
		if isNew {
			s.ArchivePrefix = models.ArchivePrefixFor(s.Name)
			if taken, err := a.store.ArchivePrefixTaken(s.ArchivePrefix, 0); err != nil {
				a.serverError(w, r, err)
				return
			} else if taken {
				errs = append(errs, "error.archive_prefix_taken")
			}
		}
	}
	if s.IsLocutusClient() && s.Host != "" {
		others, err := a.store.ContainerSystems(&models.Container{Host: s.Host, Port: s.Port})
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		for _, o := range others {
			switch {
			case o.ID == s.ID:
			case s.IsClient() && o.IsClient():
				errs = append(errs, "error.container_second_client")
			case s.IsRepository() && o.IsRepository() && o.RepoPath == s.RepoPath:
				errs = append(errs, "error.container_repo_taken")
			}
		}
	}
	// Secrets: an empty field keeps the stored value.
	sshKey, keyErr := formPrivateKey(r, "ssh_key")
	if keyErr != "" {
		errs = append(errs, keyErr)
	}
	var pruneKey string
	if s.IsRepository() {
		if pruneKey, keyErr = formPrivateKey(r, "prune_ssh_key"); keyErr != "" {
			errs = append(errs, keyErr)
		}
	}
	passphrase := r.FormValue("passphrase")
	if len(errs) > 0 {
		data, err := a.systemFormData(s)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		a.renderSystemForm(w, r, http.StatusUnprocessableEntity, data, a.translateAll(r, errs))
		return
	}
	if err := a.store.SaveSystem(s); err != nil {
		a.serverError(w, r, err)
		return
	}
	var err error
	switch {
	case r.FormValue("remove_ssh_key") == "1":
		err = a.store.SetSystemSSHKey(s.ID, "")
	case sshKey != "":
		err = a.store.SetSystemSSHKey(s.ID, sshKey)
	}
	if err == nil && s.IsRepository() {
		switch {
		case r.FormValue("remove_prune_ssh_key") == "1":
			err = a.store.SetSystemPruneSSHKey(s.ID, "")
		case pruneKey != "":
			err = a.store.SetSystemPruneSSHKey(s.ID, pruneKey)
		}
	}
	if err == nil && s.IsRepository() {
		switch {
		case r.FormValue("remove_passphrase") == "1":
			err = a.store.SetSystemPassphrase(s.ID, "")
		case passphrase != "":
			err = a.store.SetSystemPassphrase(s.ID, passphrase)
		}
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if isNew {
		a.pushAffected(r, s)
	} else {
		a.pushAffected(r, &before, s)
	}
	a.flash(w, r, "success", "admin.systems.saved", s.Name)
	http.Redirect(w, r, "/admin/systems/"+itoa(s.ID)+"/edit", http.StatusSeeOther)
}

// formPrivateKey reads a pasted private key from the form. It returns "" for
// an empty field, or an error message key when the key cannot be used.
func formPrivateKey(r *http.Request, field string) (key, errKey string) {
	key = strings.TrimSpace(strings.ReplaceAll(r.FormValue(field), "\r\n", "\n"))
	if key == "" {
		return "", ""
	}
	key += "\n"
	if _, err := ssh.ParseRawPrivateKey([]byte(key)); err != nil {
		if _, ok := err.(*ssh.PassphraseMissingError); ok {
			return "", "error.ssh_key_encrypted"
		}
		return "", "error.ssh_key_invalid"
	}
	return key, ""
}

// adminSystemUsers assigns users (any role except admin) to a system.
func (a *App) adminSystemUsers(w http.ResponseWriter, r *http.Request) {
	s := a.systemFromPath(w, r)
	if s == nil {
		return
	}
	var ids []int64
	for _, id := range formIDs(r, "user") {
		if u, err := a.store.GetUser(id); err == nil && u.Role != models.RoleAdmin {
			ids = append(ids, id)
		}
	}
	if err := a.store.SetSystemUsers(s.ID, ids); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.flash(w, r, "success", "admin.systems.users_saved", s.Name)
	http.Redirect(w, r, "/admin/systems/"+itoa(s.ID)+"/edit#users", http.StatusSeeOther)
}

// adminSystemGenerateKey creates a new ed25519 key pair for the system. The
// admin only gets to see the public key, to install it on the other side.
// With key=prune it replaces the separate prune key of a repository.
func (a *App) adminSystemGenerateKey(w http.ResponseWriter, r *http.Request) {
	s := a.systemFromPath(w, r)
	if s == nil {
		return
	}
	prune := r.FormValue("key") == "prune"
	if prune && !s.IsRepository() {
		a.errorPage(w, r, http.StatusBadRequest, "error.not_found")
		return
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	comment, anchor := "locutus "+s.Name, "#ssh"
	if prune {
		comment, anchor = "locutus-prune "+s.Name, "#prune-key"
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	key := string(pem.EncodeToMemory(block))
	if prune {
		err = a.store.SetSystemPruneSSHKey(s.ID, key)
	} else {
		err = a.store.SetSystemSSHKey(s.ID, key)
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.pushAffected(r, s)
	a.flash(w, r, "success", "admin.systems.key_generated")
	http.Redirect(w, r, "/admin/systems/"+itoa(s.ID)+"/edit"+anchor, http.StatusSeeOther)
}

// adminSystemTest checks the connection: borg info for a repository (also
// with the prune key, when one is set), the borg version for a client.
func (a *App) adminSystemTest(w http.ResponseWriter, r *http.Request) {
	s := a.systemFromPath(w, r)
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	var out, pushed string
	var err error
	// A Locutus client container first gets the current configuration.
	if c, cerr := a.store.ContainerOf(s); cerr != nil {
		a.serverError(w, r, cerr)
		return
	} else if c != nil {
		pushed, err = a.pushContainer(ctx, c, a.tr.Language(r))
		switch {
		case errors.Is(err, errNoManagerKey):
			pushed, err = "", nil
		case err != nil:
			a.renderTestResult(w, r, s, a.t(r, "container.push_title")+"\n"+pushed, err)
			return
		default:
			pushed = a.t(r, "container.push_title") + "\n" + pushed + "\n\n" + a.t(r, "container.test_title") + "\n"
		}
	}
	if s.IsRepository() {
		var repo *borg.Repo
		if repo, err = scheduler.RepoFor(a.store, s); err == nil {
			out, err = a.borg.Info(ctx, repo)
			if err == nil && repo.PruneSSHKey != "" {
				var pruneOut string
				pruneOut, err = a.borg.Info(ctx, repo.ForPrune())
				out = a.t(r, "admin.systems.test_main_key") + "\n" + out + "\n" +
					a.t(r, "admin.systems.test_prune_key") + "\n" + pruneOut
				if err != nil {
					out = strings.TrimSpace(out) + "\n" + a.t(r, "admin.systems.test_prune_key_failed")
				}
			}
		}
	} else {
		cl := borg.Client{Local: !s.IsRemote(), Address: s.SSHAddress(), User: s.SSHUser, Sudo: s.UseSudo}
		var sec *models.SystemSecrets
		if sec, err = a.store.SystemSecrets(s.ID); err == nil {
			cl.SSHKey = sec.SSHKey
			out, err = a.borg.ClientVersion(ctx, cl)
		}
	}
	a.renderTestResult(w, r, s, pushed+out, err)
}

func (a *App) renderTestResult(w http.ResponseWriter, r *http.Request, s *models.System, out string, err error) {
	data, derr := a.systemFormData(s)
	if derr != nil {
		a.serverError(w, r, derr)
		return
	}
	data.Tested, data.TestOK, data.TestOutput = true, err == nil, out
	if err != nil {
		data.TestOutput = strings.TrimSpace(out + "\n" + err.Error())
	}
	a.renderSystemForm(w, r, http.StatusOK, data, nil)
}

// adminSystemInit initialises a new borg repository.
func (a *App) adminSystemInit(w http.ResponseWriter, r *http.Request) {
	s := a.systemFromPath(w, r)
	if s == nil {
		return
	}
	if !s.IsRepository() {
		a.errorPage(w, r, http.StatusBadRequest, "error.not_found")
		return
	}
	mode := r.FormValue("encryption")
	if !borg.ValidEncryption(mode) {
		a.errorPage(w, r, http.StatusBadRequest, "error.encryption_invalid")
		return
	}
	if mode != "none" && !s.HasPassphrase {
		a.flash(w, r, "error", "error.passphrase_required")
		http.Redirect(w, r, "/admin/systems/"+itoa(s.ID)+"/edit", http.StatusSeeOther)
		return
	}
	repo, err := scheduler.RepoFor(a.store, s)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	out, err := a.borg.Init(ctx, repo, mode)
	a.renderTestResult(w, r, s, out, err)
}

func (a *App) adminDeleteSystem(w http.ResponseWriter, r *http.Request) {
	s := a.systemFromPath(w, r)
	if s == nil {
		return
	}
	if s.IsRepository() {
		if n, err := a.store.CountRepositoryClients(s.ID); err != nil {
			a.serverError(w, r, err)
			return
		} else if n > 0 {
			a.flash(w, r, "error", "admin.systems.repo_in_use", s.Name)
			http.Redirect(w, r, "/admin/systems", http.StatusSeeOther)
			return
		}
	}
	if a.sched.IsRunning(s.ID) {
		a.flash(w, r, "error", "error.job_running")
		http.Redirect(w, r, "/admin/systems", http.StatusSeeOther)
		return
	}
	if err := a.store.DeleteSystem(s.ID); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.pushAffected(r, s)
	a.flash(w, r, "success", "admin.systems.deleted", s.Name)
	http.Redirect(w, r, "/admin/systems", http.StatusSeeOther)
}
