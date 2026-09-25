# garm-provider-orbstack

A [GARM](https://github.com/cloudbase/garm) external provider that runs ephemeral GitHub Actions Linux runners as [OrbStack](https://orbstack.dev) machines on a Mac — plus the native macOS installer that runs the GARM controller as a launchd service.

Status: **implementation complete; runtime acceptance gates against a published release are not yet complete.** See *Verification status*.

## What this is

- **Native macOS GARM service.** The GARM controller (`garm`) runs as a launchd LaunchAgent under the OrbStack owner account — no Docker, no SSH, no inbound webhook. GARM executes `garm-provider-orbstack` directly as its external provider (interface `v0.1.0`), and the provider drives OrbStack through `orbctl`.
- **Ephemeral copy-on-write runners.** Each runner is a clone of a sealed, immutable, credential-free Ubuntu 24.04 template machine: per-clone CPU/memory/disk limits and isolation are applied while the clone is still stopped, before first boot. Job workspaces and guest Docker state die with the machine.
- **Optional full ubuntu-latest parity.** `template build --variant full` applies the pinned official [actions/runner-images](https://github.com/actions/runner-images) Ubuntu 24.04 arm64 toolset (multi-version toolcaches, browsers, CLIs, buildx/compose) so workflows behave like `ubuntu-latest` with no changes; builds take 1–3 h and produce a ~30–50 GB template. The default `minimal` variant stays a fast, curated set (git, curl, jq, build tools, Docker engine) and pairs well with `actions/setup-*`. Limitation: when GARM requests a newer runner version than the template caches, the seeded toolcache environment is not recreated on that clone (setup-* actions then use an in-workspace cache until the template is rebuilt).
- **Repository-free targets.** A target Mac needs only macOS + OrbStack 2.2.3+. `install.sh --version vX.Y.Z` downloads checksummed release binaries (helper, provider, `garm`, `garm-cli` — built from the pinned upstream tag) and nothing else.

## Install (from a release)

```sh
sh install.sh --version vX.Y.Z        # downloads + verifies, then runs: garm-orbstack install
garm-orbstack doctor                  # exit 2 until a template is registered
garm-orbstack template build --arch arm64 \
    --runner-version <ver> --runner-sha256 <sha>
garm-orbstack doctor                  # exit 0
```

Then import a GitHub App credential and create the reference scale set (printed by the installer, operator-executed):

```sh
garm-orbstack garm-cli ... # scoped wrapper (isolated profile home, installation CA)
```

Runners reach the controller at `https://host.orb.internal:9997`; the controller binds `127.0.0.1` only.

## Security model

- **Trusted-repository scope.** All OrbStack machines share one Linux kernel; isolated machines remove Mac filesystem/SSH integration but this is **not** a hostile multi-tenant VM boundary. Network-isolation settings are defense in depth, not host/LAN separation.
- **ID-addressed lifecycle.** The provider never adopts or deletes machines by name. Known OrbStack 2.2.3 defect: `orbctl delete <ID>` segfaults in every form, so deletion uses a user-approved rename-composition (ID-addressed rename to a fresh unguessable name, binding re-verified, then delete of that exact name).
- **Crash-safe reservations.** Runner state lives in an operator-owned, fsynced, flock-locked registry; a clone in flight holds its lock for the `orbctl` child's whole lifetime (verified by killing the helper mid-clone). Uncertain outcomes require explicit `garm-orbstack recover` — never absence-based success.
- **Credential hygiene.** No GitHub credentials are created or stored by the installer; the helper never sees the GitHub App key; bootstrap JWTs are never persisted; the admin token lives only in a managed 0700 CLI home; secrets live under `~/.config/secrets/garm-orbstack`.
- **TLS.** Installation-local CA; controller cert SANs: `localhost`, `127.0.0.1`, `host.orb.internal`, `host.docker.internal`. Both CA channels are configured: the CLI transport (`GARM_CLI_CA_BUNDLE`, via a recorded one-file patch to the pinned `garm-cli` source) and the controller's runner-facing CA bundle (`garm-cli init --ca-bundle`).

## Layout

- `cmd/garm-orbstack` — operator CLI: `install`, `doctor`, `template build|list`, `recover`, `garm-cli`, `uninstall`, `version`
- `cmd/garm-provider-orbstack` — the external provider GARM executes
- `internal/orbstack` — the sole `orbctl` adapter
- `internal/state` — runner registry (records, locks, snapshots)
- `internal/provider` — lifecycle + bootstrap injection
- `internal/templates` + `images/ubuntu-24.04` — sealed template builder and recipes (embedded in release binaries)
- `internal/install` — installer, launchd service, TLS, doctor, wrapper, uninstall
- `patches/garm-cli-ca-bundle-env.patch` — the single auditable upstream patch (sha recorded in `release-lock.json`)
- `.github/workflows/release.yml` — macOS-runner prerelease pipeline (unpatched `garm`/`garm-cli` from the pinned upstream tag, both darwin arches, `SHA256SUMS`, `release-lock.json`)

## OrbStack licensing

OrbStack requires a paid license for commercial/freelance/business use. This project neither includes nor redistributes an OrbStack license; recipes are the portable source of truth.

## Verification status

Verified on this repository's development host (OrbStack 2.2.3, darwin/arm64, Go 1.26): full capability probe (`GARM_ORBSTACK_INTEGRATION=1 go test -tags=integration ./test/integration -run TestOrbStackCapabilities` — clone semantics, per-clone identity, stdin/exit propagation, root-cgroup resource limits, guest-local Docker, CA-validated HTTPS from an isolated clone to a Mac loopback listener via `host.orb.internal`, lock inheritance across a killed helper with observed live orphaned clone), unit suites including `-race`, negative install smoke, `actionlint`, `sh -n`.

**Not yet verified:** gates 3–9 against a published checksummed release (real installation, real GitHub job via a scale set, failure/recovery drills, performance, repository-free target acceptance, anonymous artifact downloads). No stable release exists yet; the first candidate is published as a prerelease and promoted only after those gates pass.

## License

Apache-2.0 (see `LICENSE`). GARM is upstream software by Cloudbase (Apache-2.0); OrbStack is separate proprietary software.
