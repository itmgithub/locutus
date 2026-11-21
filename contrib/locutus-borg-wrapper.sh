#!/bin/sh
# locutus-borg-wrapper — optional forced command for the Locutus SSH key on a
# client. Locutus also works without it; the wrapper makes sure the key can
# only be used for what Locutus does.
#
# Install as /usr/local/bin/locutus-borg-wrapper (mode 0755, owned by root)
# and reference it in ~/.ssh/authorized_keys of the backup user:
#
#   command="/usr/local/bin/locutus-borg-wrapper",restrict,agent-forwarding ssh-ed25519 AAAA... locutus
#
# Locutus only ever runs these commands; everything else is refused, so a
# leaked Locutus key cannot be used to get a shell on the client. "sudo -n "
# is only used when the client is set up with sudo in Locutus.
#
#   env BORG_...=... [sudo -n ]borg create ...                make a backup
#   env BORG_...=... [sudo -n ]borg --version                 test the connection
#   LC_ALL=C xargs -0 -r [sudo -n ]stat -c '%f %n' --         restore: which files exist
#   [sudo -n ]tar -x -p -f - -C /                             restore: write the files
#
# Start the wrapper with --no-restore to refuse restores to this client:
#
#   command="/usr/local/bin/locutus-borg-wrapper --no-restore",restrict,agent-forwarding ssh-ed25519 AAAA... locutus

set -eu

cmd="${SSH_ORIGINAL_COMMAND:-}"
allow_restore=yes
for arg in "$@"; do
    case "$arg" in
        --no-restore) allow_restore=no ;;
        *) echo "locutus-borg-wrapper: unknown option $arg" >&2; exit 2 ;;
    esac
done

deny() {
    echo "locutus-borg-wrapper: command not allowed" >&2
    logger -t locutus-borg-wrapper "refused: ${cmd}" 2>/dev/null || true
    exit 126
}

# Restores: two fixed commands. File names come in on stdin, never on the
# command line.
case "$cmd" in
    "LC_ALL=C xargs -0 -r stat -c '%f %n' --" | \
    "LC_ALL=C xargs -0 -r sudo -n stat -c '%f %n' --" | \
    "tar -x -p -f - -C /" | \
    "sudo -n tar -x -p -f - -C /")
        [ "$allow_restore" = yes ] || deny
        case "$cmd" in
            *tar*) logger -t locutus-borg-wrapper "restore: ${cmd}" 2>/dev/null || true ;;
        esac
        exec /bin/sh -c "$cmd"
        ;;
esac

# No shell meta characters: Locutus quotes arguments with single quotes and
# never sends any of these.
case "$cmd" in
    *[\;\&\|\`\$\<\>\(\)\{\}\\]* | *"
"*) deny ;;
esac

# Allowed: env BORG_X=value ... [sudo -n ]borg create ... | borg --version
if ! printf '%s\n' "$cmd" | grep -Eq '^env( BORG_[A-Z_]+=[A-Za-z0-9]+)+ (sudo -n )?borg (create( |$)|--version$)'; then
    deny
fi

exec /bin/sh -c "$cmd"
