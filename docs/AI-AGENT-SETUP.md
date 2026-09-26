# AI Agent Setup Guide

This document is for AI agents (Claude, GPT, Copilot Workspace, etc.) that need to install, operate, or uninstall garm-provider-orbstack on a macOS host. It assumes programmatic execution via shell commands.

> **TTY requirement**: The installer and recovery-login prompts read the admin password only from a TTY. Piping stdin will NOT work. For non-interactive automation, use `ssh -tt` with an `expect` script, or drive a PTY directly.

> **Admin password custody**: The installer never stores the GARM admin password. Both upgrade (`--confirm-version-change`) and reinstall prompt for it. The operator MUST store it in a secret manager (e.g. a password manager or `~/.config/secrets/`). Losing it means the preserved controller database cannot be logged into; the only recovery is destructive re-initialization.

## Prerequisites

Before installing, verify ALL of these:

```sh
# 1. macOS on Apple Silicon or Intel
uname -s   # must print Darwin
uname -m   # arm64 (native) or x86_64 (emulated)

# 2. OrbStack 2.2.3+ installed and running
orb version          # must report >= 2.2.3
orbctl status        # must print Running

# 3. Network access (for release downloads and guest packages)
curl -fsSL -o /dev/null https://github.com
```

Docker CLI and Compose are NOT required — the controller runs as a native launchd service, not in containers.

If any check fails, stop and report the missing prerequisite. Do not attempt to install OrbStack — that is an operator action.

## Installation

```sh
# Download and verify the release
sh install.sh --version v0.2.2
```

This automatically:
1. Downloads all four binaries (`garm-orbstack`, `garm-provider-orbstack`, `garm`, `garm-cli`) plus `release-lock.json` and `SHA256SUMS`
2. Verifies every SHA-256 checksum
3. Runs `garm-orbstack version` (must match the requested version)
4. Invokes `garm-orbstack install`

The installer prompts for a GARM admin username, email, and password via TTY. For expect-based automation:

```sh
# The prompts appear in this order:
#   Username: (any string, e.g. "admin")
#   Email:    (must contain @, e.g. "agent@localhost")
#   Password: (typed twice, must meet zxcvbn strength)
#
# Example expect pattern (NEVER write the password to disk):
# expect -re {Username} { send -- "admin\r"; exp_continue }
# expect -re {Password} { send -- "$env(GARM_PW)\r"; exp_continue }
```

After installation, the CLI is at `~/.local/bin/garm-orbstack`.

## Post-install verification

```sh
~/.local/bin/garm-orbstack doctor
echo "exit=$?"
# exit=0: fully operational
# exit=1: something is broken — read the output
# exit=2: healthy but no runner template registered yet
```

## Building a runner template

Templates are immutable, sealed machine images. Runners are clones of templates.

### Minimal variant (~1 minute, ~4 GB)

```sh
# Get the runner version and checksum from GitHub
RUNNER_INFO=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest)
RUNNER_VER=$(echo "$RUNNER_INFO" | python3 -c "import json,sys; print(json.load(sys.stdin)['tag_name'].lstrip('v'))")
RUNNER_SHA=$(echo "$RUNNER_INFO" | python3 -c "
import json,sys
r=json.load(sys.stdin)
a=[x for x in r['assets'] if 'linux-arm64-' in x['name'] and x['name'].endswith('.tar.gz')][0]
print(a.get('digest','').split(':',1)[-1])
")

# Build and register
~/.local/bin/garm-orbstack template build \
  --arch arm64 \
  --runner-version "$RUNNER_VER" \
  --runner-sha256 "$RUNNER_SHA"
```

### Full variant (~2-3 hours, ~30-50 GB, ubuntu-latest parity)

Add `--variant full` to the above. Requires a mostly unused hourly `api.github.com` quota (~25-40 of the 60 calls/hour limit shared by your IP). Never place a GitHub token inside the guest — it would be sealed into the template.

### Verify

```sh
~/.local/bin/garm-orbstack template list    # shows image IDs
~/.local/bin/garm-orbstack doctor           # should now exit 0
```

## Connecting to GitHub

### 1. Create a GitHub App (operator action, browser required)

- Go to https://github.com/settings/apps/new
- Set any name, any homepage URL
- **Do NOT enable webhooks** (scale sets use long-polling)
- Permissions: Repository → Actions (Read), Administration (Read/Write), Metadata (Read)
- Install on a **private** test repository
- Download the `.pem` private key
- Record the **App ID** (from the app's settings page) and **Installation ID** (from `github.com/settings/installations/<number>`)

### 2. Import credentials

```sh
# Place the key (owned by the installing user, mode 0600)
ssh localhost 'umask 077; mkdir -p ~/.config/secrets/garm-orbstack && cat > ~/.config/secrets/garm-orbstack/github-app.pem' < ~/Downloads/<app-key>.pem

# Import the credential
~/.local/bin/garm-orbstack garm-cli github credentials add \
  --name my-app \
  --auth-type app \
  --endpoint github.com \
  --app-id "$APP_ID" \
  --app-installation-id "$INSTALL_ID" \
  --private-key-path ~/.config/secrets/garm-orbstack/github-app.pem \
  --description "CI credential"

# Delete the PEM after import (GARM stores it internally)
rm ~/.config/secrets/garm-orbstack/github-app.pem
```

### 3. Add a repository

```sh
~/.local/bin/garm-orbstack garm-cli repo add \
  --owner <owner> \
  --name <repo> \
  --credentials my-app \
  --random-webhook-secret
```

### 4. Create a scale set

```sh
IMAGE_ID=$(~/.local/bin/garm-orbstack template list | python3 -c "
import json,sys
imgs = json.load(sys.stdin)
print([i['image_id'] for i in imgs if i['arch']=='arm64'][0])
")

~/.local/bin/garm-orbstack garm-cli scaleset create \
  --name orbstack-linux-arm64 \
  --repo <owner>/<repo> \
  --provider-name orbstack \
  --image "$IMAGE_ID" \
  --flavor default \
  --runner-install-template github_linux \
  --os-type linux \
  --os-arch arm64 \
  --min-idle-runners 0 \
  --max-runners 2 \
  --enabled
```

### 5. Add a workflow and trigger

```yaml
# .github/workflows/test.yml in the test repository
name: Test
on: [workflow_dispatch]
jobs:
  test:
    runs-on: orbstack-linux-arm64
    steps:
      - uses: actions/checkout@v4
      - run: echo "Running on $(uname -m)"
```

```sh
gh workflow run test.yml --repo <owner>/<repo>
```

## Everyday operations

```sh
# Health check
~/.local/bin/garm-orbstack doctor

# List templates
~/.local/bin/garm-orbstack template list

# Retire a template (refuses if runners still use it)
~/.local/bin/garm-orbstack template remove <image-id>

# Recover a stuck reservation
~/.local/bin/garm-orbstack recover --runner-name <name> --release

# Scoped garm-cli (isolated profile, installation CA)
~/.local/bin/garm-orbstack garm-cli scaleset list --repo <owner>/<repo>
~/.local/bin/garm-orbstack garm-cli runner list --repo <owner>/<repo>
```

## Uninstallation

### Step 1: Clean up GitHub side

```sh
# Disable and delete scale sets (prevents orphaned GitHub-side entities)
~/.local/bin/garm-orbstack garm-cli scaleset list --repo <owner>/<repo>
# For each scale set ID:
~/.local/bin/garm-orbstack garm-cli scaleset update <id> --enabled=false
# Wait for runners to drain, then:
~/.local/bin/garm-orbstack garm-cli scaleset delete <id>

# Remove the repository and credentials
~/.local/bin/garm-orbstack garm-cli repo delete <owner>/<repo>
~/.local/bin/garm-orbstack garm-cli github credentials delete my-app
```

### Step 2: Retire templates

```sh
# List and remove each template (refuses if runners still reference it)
~/.local/bin/garm-orbstack template list
~/.local/bin/garm-orbstack template remove <image-id>
# Repeat for each image
```

### Step 3: Uninstall

```sh
~/.local/bin/garm-orbstack uninstall
```

This removes the launchd LaunchAgent, installed binaries, and the `~/.local/bin/garm-orbstack` symlink. It preserves the controller database, registry, secrets, and host configuration.

### Step 4 (optional): Full cleanup

Only after steps 1-3, if you want to remove everything:

```sh
# Remove any remaining runner machines (by exact name from the registry, not by prefix)
# Check the registry for any machines you created (template remove should have cleaned templates):
cat ~/.local/share/garm-orbstack/state/registry/*.json 2>/dev/null | python3 -c "
import json,sys
for line in sys.stdin:
    try:
        r = json.loads(line)
        if r.get('machine_name'):
            print(r['machine_name'])
    except: pass
"
# Then check OrbStack for any machines the registry references:
orbctl list --format json
# Delete ONLY machines you recognize from the registry above, by exact name:
orbctl delete --force <exact-machine-name-from-registry>

# Remove managed directories
rm -rf ~/.local/share/garm-orbstack ~/.config/garm-orbstack ~/.config/secrets/garm-orbstack
```

## Reinstalling after uninstall

```sh
sh install.sh --version v0.2.2
```

The installer detects the preserved database and prompts for recovery login (the existing admin password, via TTY). It never re-initializes the database.

## Upgrading

```sh
sh install.sh --version v0.2.3 --confirm-version-change
```

This stops the controller, takes a backup of the database, config, secrets, and registry, then upgrades. The admin password is prompted via TTY.

## Known limitations

- **Quota-stuck runners**: a machine that fills its `disk_bytes` quota cannot be deleted (btrfs ENOSPC during subvolume cleanup). Fix: raise the cap first (`orbctl config set machine.<name>.disk_bytes 107374182400`), then delete. The provider's delete path needs this fix in a future release
- **Docker container layers**: NOT capped by per-machine `disk_bytes` (separate btrfs subvolumes). One job can fill the shared OrbStack volume. Monitor free space; use trusted repositories only
- **Kind clusters**: not supported (OrbStack guest `/sys` remount privilege limitation)
- **Buildx `docker-container` driver**: works via native snapshotter (slower than overlay)
- **OrbStack `delete --by-ID`**: segfaults on 2.2.3; worked around internally via rename-composition
- **Self-hosted runners**: only use with **private** repositories (GitHub's recommendation)
- **Kernel sharing**: all OrbStack machines share one Linux kernel — not a hostile-tenant boundary

## Troubleshooting

| Symptom | Check | Fix |
|---|---|---|
| Doctor exits 1 | Read the specific "FAIL" line | Address the named check |
| Doctor exits 2 | `template list` is empty | Build a template |
| Jobs stay queued | `garm-cli scaleset list` — image points to a removed template | Rebuild or repoint the scale set |
| `install.sh` fails checksum | Corrupted download or MITM | Re-download; verify URL is https://github.com |
| Runner creation fails | `garm-cli runner list` for error details | Check OrbStack is running; check `--max-runners` on the scale set |
| Full template build 403s | `api.github.com` quota exhausted | Wait for the hourly window; or use a token (never inside the guest) |
| Reinstall can't log in | Admin password lost | Destructive: delete DB, remove managed dirs, reinstall from scratch |
