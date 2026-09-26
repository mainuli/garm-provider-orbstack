#!/bin/sh
# Stop automated package work and remove instance identity before stopping the
# machine. The same script is used for native ARM64 and emulated AMD64.
set -eu
umask 077
for unit in apt-daily.timer apt-daily-upgrade.timer; do
    if systemctl cat "$unit" >/dev/null 2>&1; then
        systemctl disable --now "$unit"
        systemctl mask "$unit"
    fi
done
for unit in apt-daily.service apt-daily-upgrade.service unattended-upgrades.service; do
    if systemctl cat "$unit" >/dev/null 2>&1; then
        systemctl disable "$unit"
        systemctl mask "$unit"
    fi
done
# Do not interrupt a package transaction in progress. Fail the build rather than
# sealing an image with an incomplete dpkg operation.
remaining=600
for unit in apt-daily.service apt-daily-upgrade.service; do
    while systemctl is-active --quiet "$unit"; do
        [ "$remaining" -gt 0 ] || { printf '%s\n' 'Automatic package work still active; refusing to seal' >&2; exit 1; }
        sleep 1
        remaining=$((remaining - 1))
    done
done
while fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/lib/apt/lists/lock /var/cache/apt/archives/lock >/dev/null 2>&1; do
    [ "$remaining" -gt 0 ] || { printf '%s\n' 'Package manager still active; refusing to seal' >&2; exit 1; }
    sleep 1
    remaining=$((remaining - 1))
done
# The unattended-upgrades shutdown monitor can stay active while idle. Its
# package work has finished above; stopping the monitor does not kill dpkg.
if systemctl is-active --quiet unattended-upgrades.service; then
    systemctl stop unattended-upgrades.service
fi
# OrbStack's 24.04 base has no cloud-init; preserve that property if a later base
# includes it, without assuming an absent service is an error.
install -d -m 0755 /etc/cloud
touch /etc/cloud/cloud-init.disabled
for unit in cloud-init-local.service cloud-init.service cloud-config.service cloud-final.service; do
    if systemctl cat "$unit" >/dev/null 2>&1; then
        systemctl disable "$unit"
        systemctl mask "$unit"
    fi
done
# A build is never allowed to turn an already registered runner into a template.
for file in /home/runner/actions-runner/.runner /home/runner/actions-runner/.credentials* /home/runner/actions-runner/.service; do
    [ ! -e "$file" ] || { printf '%s\n' 'Registered runner material found; refusing template' >&2; exit 1; }
done
if find /etc/systemd/system -name 'actions.runner.*' -print -quit | /bin/grep -q .; then
    printf '%s\n' 'Registered runner service found; refusing template' >&2
    exit 1
fi
rm -rf /home/runner/actions-runner/_work /home/runner/actions-runner/_diag \
    /home/runner/.ssh /root/.ssh /run/garm /var/lib/cloud \
    /home/runner/.cache /root/.cache
rm -f /home/runner/.bash_history /root/.bash_history /etc/ssh/ssh_host_* \
    /var/log/garm-bootstrap.log /etc/systemd/system/garm-bootstrap.service
# Best-effort: some toolset logs (e.g. postgresql's) deny writes even to
# root under OrbStack; truncate when possible, else remove, else leave (the
# sensitive-wipe already removed credentials elsewhere).
find /var/log -type f -exec sh -c 'truncate -s 0 "$1" 2>/dev/null || rm -f "$1" 2>/dev/null || true' sh {} \;
# Preserve build inputs until this shell exits; the builder removes the directory
# using a separate command. All metadata under /etc/garm-template is nonsecret.
rm -f /var/lib/dbus/machine-id
ln -s /etc/machine-id /var/lib/dbus/machine-id
truncate -s 0 /etc/machine-id
sync
