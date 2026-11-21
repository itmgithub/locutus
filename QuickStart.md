# Quick start: backing up a Linux server with sudo

This guide takes you from nothing to a working, scheduled backup of one Linux server. It uses the
safest setup Locutus offers:

- Locutus logs in on the server as an ordinary user and only uses **sudo** for borg, `stat` and
  `tar`, never as root;
- the SSH key of Locutus is locked to its own commands with the **wrapper**;
- the backup server only allows **append-only** access, with a separate **prune key** that only
  Locutus has;
- people log in to Locutus with **local accounts**, and nobody works as the administrator day to
  day.

The [README](README.md) explains every step in more detail.

You need three machines (two of them can be the same):

| Name in this guide | What it is |
|--------------------|------------|
| **Locutus host** | A machine with Docker and Docker Compose that runs Locutus. |
| **Backup server** `repo.example.com` | Where borg stores the backups. Needs SSH and borg. |
| **Client** `web1.example.com` | The Linux server you want to back up. Needs SSH, sudo and borg. |

The client must be able to reach the backup server over SSH. Use the same major version of borg
(1.x) everywhere. Replace the host names, the IP address `192.0.2.10` (the address Locutus uses
to reach the backup server) and `web1` with your own.

## 1. Install Locutus

On the Locutus host, in the directory with the Locutus files:

1. Edit `docker-compose.yml`:
   - set `TZ` to your time zone, for example `TZ=Europe/Amsterdam`;
   - set `LOCUTUS_BASE_URL` to the address you open Locutus at, or remove that line;
   - enable HTTPS: put a certificate and key in `./certs` and set `LOCUTUS_TLS_CERT` and
     `LOCUTUS_TLS_KEY`. You can skip this only when Locutus sits behind a reverse proxy that
     handles HTTPS.
2. Start it:

   ```sh
   docker compose up -d --build
   ```

3. Open `https://<host>:3000` and create the administrator. Choose a long, unique password and
   keep it in a password manager.

Locutus now has a `secret.key` in `./data`. Without that file the SSH keys and passphrases stored
in Locutus are useless, so copy it to a safe place outside the Locutus host.

## 2. Create the local users

The administrator manages systems and users but cannot see backups or files. For the daily work
you therefore create separate local accounts. Log in as the administrator, go to **Users** and
click **Add local user**:

| Account | Role | For |
|---------|------|-----|
| for example `jan` | **Backupper** | Setting up schedules, starting backups, restoring files. |
| optional | **Restorer** | Someone who may only restore. |
| optional | **Viewer** | Someone who may only look. |

Give each person their own account with a strong password; never share accounts. Only give the
Backupper and Restorer roles to people you trust to write files on the client, because with sudo
a restore can overwrite any file, including those in `/etc`.

You assign the systems to these users in [step 6](#6-assign-the-systems).

## 3. Set up the backup server

### 3.1 Create the borg user and a directory

On the backup server, as root:

```sh
apt install borgbackup             # or: dnf install borgbackup / apk add borgbackup
useradd -m -s /bin/sh borg
install -d -o borg -m 700 /srv/borg ~borg/.ssh
```

### 3.2 Create the repository in Locutus

As the administrator, go to **Systems › New repository**:

- choose *Remote*;
- host `repo.example.com`, SSH user `borg`, path `/srv/borg/web1`;
- enter a long, random **passphrase**. Store it in your password manager as well.

Save, then click **Generate new key pair** and **Generate prune key pair**. Locutus shows both
public keys.

Use one repository per client. Anyone with root on a client can use the repository key during a
backup, so clients should not share a repository.

### 3.3 Allow the keys

Put both keys in `~borg/.ssh/authorized_keys` on the backup server, each on one line:

```
# Normal key: append-only, only this repository
command="borg serve --append-only --restrict-to-repository /srv/borg/web1",restrict ssh-ed25519 AAAA... locutus

# Prune key: may delete, but only from the IP address of Locutus
from="192.0.2.10",command="borg serve --restrict-to-repository /srv/borg/web1",restrict ssh-ed25519 AAAA... locutus-prune
```

Then fix the permissions:

```sh
chown borg: ~borg/.ssh/authorized_keys
chmod 600 ~borg/.ssh/authorized_keys
```

### 3.4 Initialise and export the key

In Locutus, click **Test connection** on the repository (both keys are tested), then
**Initialise** and choose `repokey-blake2`.

Then export the repository key on the backup server and keep it, together with the passphrase,
outside Locutus (for example printed in a safe, or in your password manager):

```sh
sudo -u borg borg key export /srv/borg/web1 ~borg/web1-borg-key.txt
```

Without the passphrase and this key you cannot get to your backups if Locutus is lost. Delete the
exported file from the backup server once it is stored safely.

## 4. Set up the client

### 4.1 Create the client in Locutus

As the administrator, go to **Systems › New client**:

- choose *Remote* and the repository `web1` from step 3;
- host `web1.example.com`, SSH user `borg`;
- tick **Run borg and restores with sudo**.

Save and click **Generate new key pair**. Keep the public key at hand.

### 4.2 Install borg and create the user

On the client, as root:

```sh
apt install borgbackup sudo
useradd -m -s /bin/sh borg
install -d -o borg -m 700 ~borg/.ssh
```

The user `borg` gets no password, so it can only log in with the key of Locutus.

### 4.3 Install the wrapper

Copy [`contrib/locutus-borg-wrapper.sh`](contrib/locutus-borg-wrapper.sh) to the client and
install it as root:

```sh
install -o root -g root -m 755 locutus-borg-wrapper.sh /usr/local/bin/locutus-borg-wrapper
```

Put the public key of the client in `~borg/.ssh/authorized_keys`, behind the wrapper:

```
command="/usr/local/bin/locutus-borg-wrapper",restrict,agent-forwarding ssh-ed25519 AAAA... locutus
```

```sh
chown borg: ~borg/.ssh/authorized_keys
chmod 600 ~borg/.ssh/authorized_keys
```

The key can now only make backups and restore files. If nothing may ever be restored to this
client, use `command="/usr/local/bin/locutus-borg-wrapper --no-restore"` instead.

### 4.4 Lock down sshd for this user

Add this to the end of `/etc/ssh/sshd_config`:

```
Match User borg
    AllowAgentForwarding yes
    AllowTcpForwarding no
    X11Forwarding no
    PermitTTY no
    PasswordAuthentication no
```

Check the configuration and restart sshd:

```sh
sshd -t && systemctl restart ssh     # the service is called sshd on some distributions
```

### 4.5 Allow sudo for borg, stat and tar

Check the paths of the commands:

```sh
command -v borg stat tar
```

Create the sudo rules with `visudo -f /etc/sudoers.d/locutus` and adjust the paths if they differ:

```
# Backups: borg as root. The variables carry the passphrase and the
# repository key through sudo.
Defaults!/usr/bin/borg env_keep += "BORG_PASSPHRASE_FD BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK BORG_RELOCATED_REPO_ACCESS_IS_OK SSH_AUTH_SOCK"
borg ALL=(root) NOPASSWD: /usr/bin/borg

# Restores (leave this out if nothing may be restored to this client)
borg ALL=(root) NOPASSWD: /usr/bin/stat, /usr/bin/tar -x -p -f - -C /
```

Check that sudo accepts the rules:

```sh
sudo -u borg sudo -n -l
```

These rules allow the user `borg` to run borg as root, and borg can read every file. That is why
the wrapper and the sshd restrictions above matter: only Locutus can use this user, and only for
its own commands.

### 4.6 Introduce the backup server to root

With sudo, borg runs as root, so it connects to the backup server with the SSH settings of root.
As root on the client:

```sh
install -d -m 700 /root/.ssh
ssh-keyscan -p 22 repo.example.com >> /root/.ssh/known_hosts
ssh-keygen -lf /root/.ssh/known_hosts
```

Compare the fingerprint with the one on the backup server (`ssh-keygen -lf
/etc/ssh/ssh_host_ed25519_key.pub` there). Only continue if they match.

Then create `/root/.ssh/config`, with the same host name as the repository in Locutus:

```
Host repo.example.com
    User borg
    Port 22
    IdentitiesOnly no
    IdentityAgent SSH_AUTH_SOCK
    ForwardAgent no
    ForwardX11 no
    ClearAllForwardings yes
    BatchMode yes
    StrictHostKeyChecking yes
    UpdateHostKeys no
    ServerAliveInterval 30
    ServerAliveCountMax 6
```

```sh
chmod 600 /root/.ssh/config
```

### 4.7 Test

In Locutus, click **Test connection** on the client. It should show the borg version, for example
`borg 1.4.0`, which also proves that sudo works.

Locutus remembers the host key of the client on this first connection and refuses the connection
if it ever changes. If you want to be sure it remembered the right one, compare the line for
`web1.example.com` in `ssh/known_hosts` in the Locutus data directory with
`/etc/ssh/ssh_host_*_key.pub` on the client.

## 5. Back up Locutus itself (do this now)

Losing the Locutus data directory means losing the stored SSH keys and passphrases. Make sure you
have, outside the Locutus host:

- `secret.key` from the data directory (step 1);
- the passphrase and key export of every repository (step 3.4);
- preferably a regular backup of the data directory, see
  [Backing up Locutus itself](README.md#backing-up-locutus-itself).

## 6. Assign the systems

As the administrator, go to **Systems**, open the client `web1` and, under **Assigned users**,
assign the backupper from step 2. Do the same for any restorers and viewers. Log out as the
administrator.

## 7. Schedule the backups

Log in as the backupper, open **My systems** and choose `web1`.

1. **Backup schedule**: enter the paths, for example

   ```
   /etc
   /home
   /root
   /var/www
   ```

   and excludes such as `/home/*/.cache`. Choose `zstd`, a daily time, tick **Run backups
   according to this schedule** and save.
2. Click **Back up now** and check the output under **Logging**.
3. **Prune schedule**: keep the defaults (24 hourly, 7 daily, 4 weekly, 6 monthly) and choose a
   weekly time. With an append-only repository, pruning makes earlier deletions final, so don't
   prune more often than needed. Tick the schedule and save.
4. Click **Prune now** once. The output should say
   `Using the separate prune key of the repository.`

## 8. Test a restore

A backup you have never restored from is not a backup yet. Open the newest backup with
**View / restore**, tick a file such as `/etc/hostname` and restore it:

- **To client**: `web1`;
- **Destination**: `/tmp/restore-test`;
- leave **Dry run** on, click **Start dry run** and check the result;
- go back, untick **Dry run** and click **Start restore**.

Check on the client that `/tmp/restore-test/etc/hostname` exists with the right owner and
permissions, and remove the directory again.

## Checklist

- [ ] Locutus is reachable over HTTPS only.
- [ ] Everyone has their own local account; the administrator account is only used for
      administration.
- [ ] The backup server only accepts the normal key with `--append-only` and the prune key only
      from the IP address of Locutus.
- [ ] Each client has its own repository.
- [ ] The client key is behind the wrapper, the user `borg` has no password and sshd is locked
      down for it.
- [ ] `secret.key`, the passphrases and the repository key exports are stored outside Locutus.
- [ ] A test restore has worked.
- [ ] After updating Locutus, you also install the new wrapper on the clients.

If something goes wrong, see [Troubleshooting](README.md#troubleshooting).
