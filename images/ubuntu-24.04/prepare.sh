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
apt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends \
    ca-certificates curl git jq tar gzip unzip build-essential sudo \
    libicu74 libssl3t64 zlib1g libkrb5-3 libcurl4t64 liblttng-ust1t64 \
    docker.io psmisc
id runner >/dev/null 2>&1 || useradd --create-home --shell /bin/bash runner
usermod --shell /bin/bash --append --groups docker runner
printf '%s\n' 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/garm-runner
chmod 0440 /etc/sudoers.d/garm-runner
visudo -cf /etc/sudoers.d/garm-runner
systemctl enable --now docker.service
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
