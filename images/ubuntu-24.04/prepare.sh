#!/bin/sh
# Runs only while building a NEW template, never on runner clones.
set -eu
umask 022
arch=$1
runner_version=$2
runner_sha256=$3
filename=$4
case "$arch" in arm64) github_arch=arm64 ;; amd64) github_arch=x64 ;; *) exit 2 ;; esac
[ "$filename" = "actions-runner-linux-${github_arch}-${runner_version}.tar.gz" ]
[ "$(dpkg --print-architecture)" = "$arch" ]
. /etc/os-release
[ "$ID" = ubuntu ] && [ "$VERSION_ID" = 24.04 ]
export DEBIAN_FRONTEND=noninteractive
apt-get -o DPkg::Lock::Timeout=600 update
# overlay2 cannot manage whiteouts on OrbStack's guest filesystem (EIO
# "failed to register layer"/unlinkat during pulls and builds on
# node-based images). Pin the btrfs storage driver before dockerd's first
# start; docker.io's containerd image store ignores a plain storage-driver
# pin (it stays on overlayfs), so disable the snapshotter too. Verified on
# OrbStack 2.2.3: btrfs driver handles whiteout builds, node-based image
# pulls and save/load round trips inside machine clones.
install -d /etc/docker
[ -f /etc/docker/daemon.json ] || printf '{"storage-driver":"btrfs","features":{"containerd-snapshotter":false}}\n' > /etc/docker/daemon.json

# daemon.json must exist before the docker.io postinst starts dockerd.
apt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends \
    ca-certificates curl git jq tar gzip unzip build-essential sudo \
    libicu74 libssl3t64 zlib1g libkrb5-3 libcurl4t64 liblttng-ust1t64 \
    docker.io psmisc
# BuildKit's docker-container driver uses overlayfs internally, which is
# not permitted on OrbStack guests (same filesystem limit as the btrfs pin
# above). The native snapshotter (which copies layers instead of overlaying)
# works. buildx reads this file when creating docker-container builders
# without an explicit --buildkitd-config, which is how docker/setup-buildx-action
# creates its builders. Listed explicitly so the parent gets the right owner.
install -d -m 0755 -o runner -g runner /home/runner/.docker /home/runner/.docker/buildx
printf '[worker.oci]\nsnapshotter = "native"\n' > /home/runner/.docker/buildx/buildkitd.default.toml
chown runner:runner /home/runner/.docker/buildx/buildkitd.default.toml

id runner >/dev/null 2>&1 || useradd --create-home --shell /bin/bash runner
usermod --shell /bin/bash --append --groups docker runner
printf '%s\n' 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/garm-runner
# Rootless podman needs subuid/subgid ranges for the runner user
# (upstream install-container-tools.sh sets these for the build user only)
printf 'runner:100000:65536\n' >> /etc/subuid
printf 'runner:100000:65536\n' >> /etc/subgid
chmod 0440 /etc/sudoers.d/garm-runner
visudo -cf /etc/sudoers.d/garm-runner
systemctl enable --now docker.service
# The storage pin must be effective: a dockerd that rejects the config or
# ignores it (containerd store on) fails here instead of shipping a
# template whose clones hit whiteout EIO on the first node-based image.
docker info --format '{{.Driver}}' | grep -qx btrfs
install -d -m 0755 -o runner -g runner /home/runner/actions-runner
curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
    "https://github.com/actions/runner/releases/download/v${runner_version}/${filename}" \
    --output /var/tmp/garm-template-build/runner.tar.gz
printf '%s  %s\n' "$runner_sha256" /var/tmp/garm-template-build/runner.tar.gz | sha256sum --check --strict -
tar -xzf /var/tmp/garm-template-build/runner.tar.gz -C /home/runner/actions-runner
/home/runner/actions-runner/bin/installdependencies.sh
chown -R runner:runner /home/runner/actions-runner
actual_version=$(su -l -c '/home/runner/actions-runner/bin/Runner.Listener --version' runner)
[ "$actual_version" = "$runner_version" ] || { printf '%s\n' 'Runner version does not match verified archive' >&2; exit 1; }
rm -f /var/tmp/garm-template-build/runner.tar.gz
