# Quick start: backing up with the Locutus client container

This guide takes you from nothing to a working, scheduled backup of a machine that runs Docker:
a server, a NAS, a Raspberry Pi, or a host with the volumes of other containers. The machine
itself only needs Docker: the Locutus client container does the rest, and Locutus configures it.
As an example, this guide backs up the shares of a NAS. It uses the same safe setup as the
[QuickStart for a Linux server](../QuickStart.md):

- Locutus logs in on the container as the ordinary user `borg` and only uses **sudo** for borg,
  `stat` and `tar`, never as root;
- every SSH key of Locutus can do exactly one thing, and the client key is locked to its own
  commands with the **wrapper**;
- the backup server only allows **append-only** access, with a separate **prune key** that only
  works from Locutus;
- the container keeps no database: only its SSH host key, the configuration that Locutus pushed
  and the borg cache are stored in `/config`.

The container can play both parts of a backup: the **repository role** makes it a backup server,
the **client role** lets Locutus back up the directories you mount into it. Use one of them or
both. You don't set the roles in the container: in Locutus, you create the repositories and the
client with the location **Locutus client**, and Locutus pushes the configuration to the
container with its **management key**. The [README](../README.md#the-locutus-client-container)
explains more.

You need three machines (two or all three of them can be the same):

| Name in this guide | What it is |
|--------------------|------------|
| **Locutus host** | A machine that runs Locutus, set up with steps 1 and 2 of the [QuickStart](../QuickStart.md). |
| **Backup server** `repo.example.com` | Where borg stores the backups: the client container in the repository role. |
| **Client** `nas.example.com` | The machine whose data you want to back up, in this guide a NAS with shares: the client container in the client role. |

Locutus must be able to reach both containers over SSH, and the client must be able to reach the
backup server. Use the same version of the client container as of Locutus; the version is shown
at the start of its log, for example `Locutus client v1.2 (Build: abcde) starting`. Replace the
host names, the port `2223` and `nas` with your own.

## 1. Install the client container

Do this on every machine that runs the client container: the backup server, the client, or both.

### 1.1 Get the files

You need [`docker-compose.yml`](docker-compose.yml) from this directory.

- **With the image from your registry**: copy only `docker-compose.yml`, remove the `build:`
  section and set `image:` to your image, for example `registry.example.com/locutus-client:v1.2`.
- **With an image file built on another machine**: in the root of the Locutus repository, run
  `./build.sh -t client`. Copy the file for the architecture of the machine, such as
  `locutus-client-arm64.tar`, together with `docker-compose.yml`, and load it with
  `docker load -i locutus-client-arm64.tar`. Then remove the `build:` section from
  `docker-compose.yml`; `image: itmon.nl/locutus-client` stays.
- **Building it on the machine itself**: clone the Locutus repository and use
  `client/docker-compose.yml` there. `docker compose build` then builds the image from the
  repository.

To build and push the image for amd64 and arm64, run this in the root of the repository:

```sh
docker buildx build --platform linux/amd64,linux/arm64 --target client \
    -t registry.example.com/locutus-client:v1.2 --push .
```

### 1.2 Choose the port and the directories

Edit `docker-compose.yml`:

- `ports:` the SSH port on the machine, by default `2223`. Locutus connects to this port.
- `./config:/config` keeps the SSH host key and the configuration from Locutus. Keep this
  directory: with a new host key, Locutus refuses the connection until you accept the new key.
- For the **repository role**: `/srv/borg:/repos`, the directory where the repositories go.
- For the **client role**: one volume per directory you want to back up, below `/volumes`. That
  can be any directory of the machine, or the volume of another container. For the shares of
  the NAS in this guide, for example

  ```yaml
      - /volume1/photos:/volumes/photos:ro
      - /volume1/documents:/volumes/documents:ro
      - /volume1/homes:/volumes/homes:ro
      - /volume1/restore:/volumes/restore
  ```

  You can mount as many as you like. With `:ro` nothing in the share can be changed, also not by
  a restore; restores then go to a writable directory such as `/volumes/restore`. Leave out
  `:ro` for the shares you want to be able to restore to their original place.

Remove the volumes of a role you don't use. Set `TZ` to your time zone, for example
`TZ: Europe/Amsterdam`. Don't start the container yet: it needs the management key from step 2.

## 2. Set up the backup server

### 2.1 Create the repository in Locutus

As the administrator, go to **Systems › New repository**:

- choose *Locutus client container*;
- host `repo.example.com`, port `2223`; the SSH user is always `borg`;
- path `/repos/nas`: always `/repos/` followed by the name of the repository, in lower case;
- enter a long, random **passphrase**. Store it in your password manager as well.

Save, then click **Generate new key pair** and **Generate prune key pair**. You don't have to do
anything with the public keys: Locutus puts them in the container itself.

Use one repository per client. Anyone with root on a client can use the repository key during a
backup, so clients should not share a repository.

### 2.2 Give the container its management key

On the same page, under **Locutus client container**, click **Generate management key**. Locutus
shows a line such as

```yaml
LOCUTUS_MANAGER_KEY: ssh-ed25519 AAAA... locutus-manager
```

Put this line in `docker-compose.yml` of the backup server, under `environment:`. Every system in
the same container (same host and port) shares this key, so you only do this once per container.

Optionally, add `LOCUTUS_MANAGER_FROM` with the IP address Locutus connects from. The management
key and the prune keys then only work from there. Without it, the prune keys work from the
address Locutus last pushed the configuration from.

### 2.3 Start and compare the host key

```sh
docker compose up -d
docker compose logs
```

The log shows the management key and the SSH host key of the container:

```
Locutus client v1.2 (Build: abcde) starting
management key: SHA256:... (from anywhere)
roles: none yet: waiting for the configuration from Locutus
SSH host key: ssh-ed25519 AAAA... (SHA256:...)
```

In Locutus, click **Check host key** under **Locutus client container**. Compare the fingerprint
with the line `SSH host key:` in the log and only continue if they match. Click **Accept host
key**: Locutus pushes the configuration right away. The log of the container then shows

```
configuration pushed by Locutus from 192.0.2.10
roles: repository server
repository nas: /repos/nas (append-only, prune key from 192.0.2.10)
```

### 2.4 Initialise and export the key

In Locutus, click **Test connection** (both keys are tested), then **Initialise** and choose
`repokey-blake2`.

Then export the repository key on the backup server and keep it, together with the passphrase,
outside Locutus (for example printed in a safe, or in your password manager):

```sh
docker compose exec -u borg locutus-client borg key export /repos/nas /tmp/nas-borg-key.txt
docker compose exec locutus-client cat /tmp/nas-borg-key.txt
docker compose exec locutus-client rm /tmp/nas-borg-key.txt
```

Without the passphrase and this key you cannot get to your backups if Locutus is lost.

## 3. Set up the client

### 3.1 Create the client in Locutus

As the administrator, go to **Systems › New client**:

- choose *Locutus client container* and the repository `nas` from step 2;
- host `nas.example.com`, port `2223`;
- tick **Allow restores to this client**, unless nothing may ever be restored to it.

Save and click **Generate new key pair**. Sudo and the wrapper are set up by the container.

### 3.2 Give the container its management key and start it

Click **Generate management key** under **Locutus client container**, put the line in
`docker-compose.yml` of the client, start it and compare and accept the host key, as in steps 2.2
and 2.3.

Locutus pushes the client key and the host key of the backup server, which it knows from step 2.
Borg on the client therefore only connects to that server.

### 3.3 Test

In Locutus, click **Test connection** on the client. It should show the configuration that was
pushed and the borg version, for example `borg 1.4.5`, which also proves that sudo works.

## 4. Both roles in one container

A machine can also be its own backup server, for example a NAS or a server with large disks,
with the repositories on its own disks. Then use both roles in the same container: create the
repository (step 2.1) and the client (step 3.1) in Locutus, both with location *Locutus client
container* and the host and port of the machine, for example `nas.example.com` and `2223`. The
container gets one management key, which you see on both systems.

The container must be able to reach itself under that host name and port, just as Locutus does.

Keep in mind that a backup on the same machine does not protect against losing that machine.
Make a second backup elsewhere for data you can't lose.

## 5. Keep the configuration safe (do this now)

Make sure you have, outside the client and the backup server:

- the passphrase and key export of every repository (step 2.4);
- `docker-compose.yml` of every client container.

Locutus can push the rest of the configuration again at any time. Keeping the `./config`
directory as well saves you from accepting a new host key.

## 6. Assign the systems

As the administrator, go to **Systems**, open the client `nas` and, under **Assigned users**,
assign the backupper (see step 2 of the [QuickStart](../QuickStart.md#2-create-the-local-users)).
Do the same for any restorers and viewers. Log out as the administrator.

## 7. Schedule the backups

Log in as the backupper, open **My systems** and choose `nas`.

1. **Backup schedule**: enter the mounted directories, for example

   ```
   /volumes/photos
   /volumes/documents
   /volumes/homes
   ```

   and excludes such as `*/#recycle` or `*/@eaDir`. Choose `zstd`, a daily time, tick **Run
   backups according to this schedule** and save.
2. Click **Back up now** and check the output under **Logging**: the number of files should be
   what you expect.
3. **Prune schedule**: keep the defaults (24 hourly, 7 daily, 4 weekly, 6 monthly) and choose a
   weekly time. With an append-only repository, pruning makes earlier deletions final, so don't
   prune more often than needed. Tick the schedule and save.
4. Click **Prune now** once. The output should say
   `Using the separate prune key of the repository.`

## 8. Test a restore

A backup you have never restored from is not a backup yet. Open the newest backup with
**View / restore**, tick a file and restore it:

- **To client**: `nas`;
- **Destination**: `/volumes/restore`;
- leave **Dry run** on, click **Start dry run** and check the result;
- go back, untick **Dry run** and click **Start restore**.

Check on the client that the file is in the restore directory, below its original path (for example
`/volume1/restore/volumes/photos/...`), with the right owner and permissions, and remove it again.

## Updating

Update the client container together with Locutus, to the same version: the wrapper in the
container must know the commands of Locutus. Get the new image in the same way as in step 1.1
(pull it from your registry, load the new image file, or `git pull` and `docker compose build`)
and run `docker compose up -d`. The host key, the configuration from Locutus, the repositories
and the borg cache are kept.

You can also throw the container away completely, including `./config`, for example to test a
new version on a clean machine. Start a new one with the same `docker-compose.yml` (the same
`LOCUTUS_MANAGER_KEY` and the same volumes), click **Check host key** in Locutus, compare it with
the log and accept it. Locutus then pushes the configuration again, also the new host key to the
clients that back up to this container. The repositories in `/repos` are served as before; only
the borg cache is built again, so the first backup takes longer.

## Troubleshooting

| Message | Cause |
|---------|-------|
| The container stops at the start with `configuration:` | A setting is wrong; the log says which one. Settings of earlier versions such as `LOCUTUS_REPOSITORY_..._KEY` are no longer used: remove them and set `LOCUTUS_MANAGER_KEY`. |
| `knownhosts: key mismatch` when pushing or testing | The container has a new host key, for example because `./config` was removed. Click **Check host key**, compare it with the log and accept it. |
| `Permission denied (publickey)` when pushing | `LOCUTUS_MANAGER_KEY` is not the management key that Locutus shows for this host and port, or `LOCUTUS_MANAGER_FROM` is not the address Locutus connects from. |
| `Host key verification failed` in a backup | Locutus did not know the host key of the backup server when it pushed. Click **Test connection** on the repository, then **Push configuration** on the client. |
| `Permission denied` when pruning | The prune key only works from the address Locutus pushed from, or from `LOCUTUS_MANAGER_FROM`. If Locutus connects from another address now, push the configuration again or correct `LOCUTUS_MANAGER_FROM`. The log of the backup server shows the address: `Accepted publickey for borg from <address>`. |
| `the client refused the restore` | **Allow restores to this client** is not ticked for the client in Locutus. |
| `Read-only file system` in a restore | The destination is a volume mounted with `:ro`. |
| `id 1000 is already used` | Choose other ids with `LOCUTUS_BORG_UID` and `LOCUTUS_BORG_GID`. |

## Checklist

- [ ] The backup server only accepts the normal key with append-only and the prune key only
      from the IP address of Locutus.
- [ ] Each client has its own repository.
- [ ] Shares that should never change are mounted with `:ro`.
- [ ] Every host key you accepted in Locutus matched the log of the container.
- [ ] The passphrases and repository key exports are stored outside Locutus.
- [ ] `docker-compose.yml` (and preferably `./config`) of every client container is kept.
- [ ] A test restore has worked.
- [ ] The client containers have the same version as Locutus.

If something goes wrong, see [Troubleshooting](#troubleshooting) above and in the
[README](../README.md#troubleshooting).
