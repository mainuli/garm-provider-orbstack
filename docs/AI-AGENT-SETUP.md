# AI Agent Setup Guide

This document is for AI agents (Claude, GPT, Copilot Workspace, etc.) that need to install, operate, or uninstall garm-provider-orbstack on a macOS host. It assumes programmatic execution via shell commands with no interactive user input.

## Prerequisites

Before installing, verify ALL of these:

```sh
# 1. macOS on Apple Silicon or Intel
uname -s   # must print Darwin
uname -m   # arm64 (native) or x86_64 (emulated)

# 2. OrbStack 2.2.3+ installed and running
orb version          # must report >= 2.2.3
orbctl status        # must print Running

# 3. Docker context is orbstack
docker context show  # must print orbstack

# 4. Docker and Compose available
docker version --format '{{.Server.Version}}'
docker compose version

# 5. Network access (for release downloads)
curl -fsSL -o /dev/null https://github.com
```

If any check fails, stop and report the missing prerequisite. Do not attempt to install OrbStack or enable Docker — those are operator actions.

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

The installer will prompt for a GARM admin username, email, and password (interactive TTY required). If running non-interactively, pre-answer via stdin:

```sh
# For expect-style automation, the prompts are:
#   Username: (any string, e.g. "admin")
#   Email:    (must contain @, e.g. "agent@localhost")
#   Password: (typed twice, must meet zxcvbn strength)
```

After installation completes, the CLI is at `~/.local/bin/garm-orbstack`.

## Post-install verification

```sh
# Doctor must exit 0 (with a template) or 2 (no template yet)
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

Add `--variant full` to the above. Requires a mostly unused hourly `api.github.com` quota (~25-40 of the 60 calls/hour limit shared by your IP).

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

### 2. Import credentials

```sh
# Place the key (must be owned by the installing user, mode 0600)
ssh localhost 'umask 077; mkdir -p ~/.config/secrets/garm-orbstack && cat > ~/.config/secrets/garm-orbstack/github-app.pem' < ~/Downloads/<app-key>.pem

# Record the App ID and Installation ID from the GitHub settings pages
APP_ID=<from github.com/settings/apps/YOUR_APP>
INSTALL_ID=<from github.com/settings/installations>

# Add the GitHub endpoint (one-time)
~/.local/bin/garm-orbstack garm-cli github endpoint list
# If empty, the github.com endpoint already exists by default

# Import the credential
~/.local/bin/garm-orbstack garm-cli github credentials add \
  --name my-app \
  --auth-type app \
  --endpoint github.com \
  --app-id "$APP_ID" \
  --app-installation-id "$INSTALL_ID" \
  --private-key-path ~/.config/secrets/garm-orbstack/github-app.pem \
  --description "CI credential"
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
# Get the image ID
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

### 5. Add a workflow to the test repository

```yaml
# .github/workflows/test.yml
name: Test
on: [workflow_dispatch]
jobs:
  test:
    runs-on: orbstack-linux-arm64
    steps:
      - uses: actions/checkout@v4
      - run: echo "Running on $(uname -m)"
```

### 6. Trigger and verify

```sh
gh workflow run test.yml --repo <owner>/<repo>
# Watch: gh run watch <run-id> --repo <owner>/<repo>
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

```sh
# Drain runners first (scale sets must be empty)
~/.local/bin/garm-orbstack garm-cli scaleset update <id> --enabled=false
# Wait for runners to finish and be deleted

# Uninstall (preserves DB, templates, secrets, host.toml)
~/.local/bin/garm-orbstack uninstall
```

What uninstall removes:
- The launchd LaunchAgent (`dev.orbstack.garm`)
- Installed release binaries and the `~/.local/bin/garm-orbstack` symlink
- The isolated garm-cli profile home

What uninstall **preserves**:
- Controller database (`~/.local/share/garm-orbstack/state/garm.db`)
- Runner registry
- Template machines and their manifests
- Secrets and TLS certificates (`~/.config/secrets/garm-orbstack/`)
- Host configuration (`~/.config/garm-orbstack/host.toml`)

To fully clean up after uninstall:

```sh
# Remove template machines
orbctl list --format json | python3 -c "
import json,sys
for m in json.load(sys.stdin):
    if m['name'].startswith('garm-template-'):
        print(m['name'])
" | xargs -I{} orbctl delete --force {}

# Remove managed directories
rm -rf ~/.local/share/garm-orbstack ~/.config/garm-orbstack ~/.config/secrets/garm-orbstack
```

## Reinstalling after uninstall

```sh
sh install.sh --version v0.2.2
```

The installer detects the preserved database and uses recovery login (never re-initializes). You will be prompted for the existing admin password.

## Known limitations

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
| Runner creation fails | `garm-cli runner list` for error details | Check OrbStack is running; check `max_instances` |
| Full template build 403s | `api.github.com` quota exhausted | Wait for the hourly window; or use a token (not in the guest) |
