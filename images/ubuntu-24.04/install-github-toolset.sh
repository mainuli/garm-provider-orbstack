#!/bin/bash
# Applies the pinned official actions/runner-images Ubuntu 24.04 arm64 toolset
# to a NEW template build (--variant full). Runs only at build time, as root,
# inside the template machine; never on runner clones. The result matches the
# software set of GitHub's hosted ubuntu-24.04 arm64 image.
#
# The builder discards guest stdout/stderr; keep the full log on the retained
# machine for post-mortem inspection.
#
# Quota note: the pinned upstream helpers make ~25-40 unauthenticated
# api.github.com calls (hard cap: 60/hour shared by everything behind the
# host IP). Start a full build only with a mostly unused hourly quota, and
# never place a GitHub token in the guest: it would be sealed into the
# template.
exec >>/var/log/garm-toolset-build.log 2>&1
set -euo pipefail

tag=$1
expected_commit=$2

[ "$(id -u)" = 0 ] || { echo 'toolset installer must run as root' >&2; exit 1; }
[ "$(dpkg --print-architecture)" = arm64 ] || { echo 'full variant recipe currently pinned to the arm64 toolset' >&2; exit 1; }

work=/var/tmp/garm-runner-images
rm -rf "$work"
mkdir -p "$work"
git clone --quiet --depth 1 --branch "$tag" https://github.com/actions/runner-images.git "$work/repo"
actual_commit=$(git -C "$work/repo" rev-parse 'HEAD^{commit}')
if [ "$actual_commit" != "$expected_commit" ]; then
    echo "actions/runner-images commit mismatch at tag $tag: $actual_commit != $expected_commit" >&2
    exit 1
fi

# Mirror packer's file provisioners exactly: helpers, installers (scripts/build),
# tests and post-generation assets all live under /imagegeneration, and the
# pinned scripts hard-code those paths (Helpers.psm1, invoke-tests.sh,
# configure-system.sh).
repo="$work/repo/images/ubuntu"
image_folder=/imagegeneration
helpers="$image_folder/helpers"
installers="$image_folder/installers"
mkdir -p "$helpers" "$installers"
cp -r "$repo/scripts/helpers/." "$helpers/"
cp -r "$repo/scripts/build/." "$installers/"
cp -r "$repo/scripts/tests" "$image_folder/tests"
cp -r "$repo/assets/post-gen" "$image_folder/post-generation"
cp "$repo/toolsets/toolset-2404-arm64.json" "$installers/toolset.json"
# The helpers fetch api.github.com unauthenticated exactly once per call; a
# full toolset needs more requests than the 60/hour shared quota, so a window
# can be exhausted mid-build and the original code fails the whole build.
# Back off across the hourly reset instead (success path unchanged).
python3 - "$helpers/install.sh" <<'PYADAPT'
import sys
path = sys.argv[1]
s = open(path).read()
old = '    json=$(curl -fsSL "https://api.github.com/repos/${repo}/releases?per_page=${page_size}")'
new = (
    '    for attempt in $(seq 1 16); do\n'
    '        json=$(curl -fsSL "https://api.github.com/repos/${repo}/releases?per_page=${page_size}") && break\n'
    '        echo "api.github.com request failed (attempt ${attempt}); waiting 300s for the quota window" >&2\n'
    '        sleep 300\n'
    '    done'
)
assert old in s, "upstream helper shape changed"
open(path, "w").write(s.replace(old, new, 1))
PYADAPT
# Several installers call api.github.com directly with a single un-retried
# curl (nvm, swift, zstd, gh-aw-firewall, docker's ecr helper); a quota 403
# silently yields null/empty and either fails the build late or installs
# nothing. Wrap every such assignment in the same window-aware retry.
python3 - "$installers" <<'PYWRAP'
import os, re, sys
installers = sys.argv[1]
for name in os.listdir(installers):
    if not name.endswith(".sh"):
        continue
    path = os.path.join(installers, name)
    lines = open(path).read().split("\n")
    changed = False
    url_vars = set()
    for line in lines:
        vm = re.match(r'^\s*([A-Za-z_][A-Za-z0-9_]*)="https://api\.github\.com[^"]*"', line)
        if vm:
            url_vars.add(vm.group(1))
    for i, line in enumerate(lines):
        if url_vars:
            varpat = "|".join(re.escape(v) for v in sorted(url_vars))
            m = re.match(r"^(\s*)([A-Za-z_][A-Za-z0-9_]*)=\$\((curl [^#]*(?:api\.github\.com|\$\{?(?:" + varpat + r")\}?)[^#]*)\)\s*$", line)
        else:
            m = re.match(r"^(\s*)([A-Za-z_][A-Za-z0-9_]*)=\$\((curl [^#]*api\.github\.com[^#]*)\)\s*$", line)
        if not m:
            continue
        indent, var, rhs = m.groups()
        wrapper = (
            f'{indent}{var}=""\n'
            f'{indent}for _garm_retry in $(seq 1 12); do\n'
            f'{indent}    {var}=$({rhs}) && [ -n "${var}" ] && [ "${var}" != "null" ] && break\n'
            f'{indent}    echo "api.github.com fetch failed (attempt ${{_garm_retry}}); waiting 300s for the quota window" >&2\n'
            f'{indent}    sleep 300\n'
            f'{indent}done'
        )
        lines[i] = wrapper
        changed = True
    if changed:
        open(path, "w").write("\n".join(lines))
        print(f"wrapped api.github.com retries in {name}")
PYWRAP

# The toolset's cmd_packages lists the virtual package "netcat", which apt
# refuses to resolve between its two providers. Install the provider the
# hosted images carry and teach the Apt pester test its command name (the
# binary is nc, via update-alternatives).
sed -i 's/"netcat"/"netcat-openbsd"/' "$installers/toolset.json"
sed -i '/"net-tools" *{ *\$toolName = "netstat"; break }/a\            "netcat-openbsd"    { $toolName = "nc"; break }' "$image_folder/tests/Apt.Tests.ps1"

export HELPER_SCRIPTS="$helpers"
# configure-system.sh reads HELPER_SCRIPT_FOLDER (pkr.hcl passes both names).
export HELPER_SCRIPT_FOLDER="$helpers"
export INSTALLER_SCRIPT_FOLDER="$installers"
export IMAGE_FOLDER="$image_folder"
export IMAGE_OS=ubuntu24
# Hosted images carry the dotted version; the tag prefix is only a git ref.
export IMAGE_VERSION="${tag#*/}"
export ARCHITECTURE=arm64
export DEBIAN_FRONTEND=noninteractive

# Documented OrbStack adaptations of Azure-VM-only logic inside otherwise
# required scripts (this machine has no cloud-init, no GRUB, a btrfs root and
# no waagent):
#   - configure-apt-sources.sh copies ubuntu.sources into /etc/cloud/templates
#     at the end; create the directory so the copy succeeds (the sealed image
#     keeps /etc/cloud for the cloud-init.disabled marker anyway).
#   - configure-environment.sh edits /etc/waagent.conf, requires an ext4 root
#     and runs update-grub; drop exactly those blocks.
install -d -m 0755 /etc/cloud/templates
sed -i -e '/waagent\.conf/d' -e '/root_fs_type=/,/update-grub$/d' "$installers/configure-environment.sh"

step() { printf '\n===== toolset step: %s =====\n' "$*" >&2; }

# Packer runs every provisioner through a fresh `sudo sh -c`, whose PAM
# session re-reads /etc/environment — that is how AGENT_TOOLSDIRECTORY,
# RUNNER_TOOL_CACHE, PIPX_* and friends written by earlier installers reach
# later ones. Mirror it per step. Upstream scripts also rely on their
# `#!/bin/bash -e` shebang; invoke with bash -e because `bash file` would
# treat the shebang as a comment and silently drop errexit.
run_step() { sudo --preserve-env=HELPER_SCRIPTS,HELPER_SCRIPT_FOLDER,INSTALLER_SCRIPT_FOLDER,IMAGE_FOLDER,IMAGE_OS,IMAGE_VERSION,ARCHITECTURE,DEBIAN_FRONTEND "$@"; }

step 'normalize the minimal LXC-style base to the cloud/server layout'
# OrbStack's Ubuntu image is distrobuilder-based: a classic one-line
# /etc/apt/sources.list on ports.ubuntu.com, no deb822 ubuntu.sources, and
# without the server seed's wget/man-db/needrestart. The pinned scripts
# assume the cloud-image layout, so normalize it first:
#   1. synthesize the deb822 sources the scripts expect. arm64 noble is
#      served only by ports.ubuntu.com (the x64 cloud image's azure.archive
#      URIs 404 for binary-arm64); upstream's azure.archive sed intentionally
#      does not match arm64 sources, so a single ports stanza is correct
#   2. install wget (install-ms-repos.sh fetches with it) and man-db
#      (configure-environment.sh reconfigures it)
#   3. patch install-ms-repos.sh's bare apt-get calls with -y (errexit +
#      no tty on stdin would abort)
if [ ! -f /etc/apt/sources.list.d/ubuntu.sources ]; then
    cat > /etc/apt/sources.list.d/ubuntu.sources <<'SOURCES'
Types: deb
URIs: http://ports.ubuntu.com/ubuntu-ports/
Suites: noble noble-updates noble-backports noble-security
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
SOURCES
    if [ -f /etc/apt/sources.list ]; then
        mv /etc/apt/sources.list /etc/apt/sources.list.d/garm-legacy-ports.list.disabled
    fi
fi
apt-get -o DPkg::Lock::Timeout=600 update
# openssl provides /etc/ssl/openssl.cnf which configure-environment.sh
# edits in place; gnupg provides gpg for the Swift key pre-import below.
apt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends wget man-db openssl gnupg
# configure-dpkg.sh force-installs the AMD64 build of libicu70 on Ubuntu
# 24.04 with no architecture guard; on arm64 that resolves amd64 libc
# dependencies and aborts with held broken packages. Restrict the block to
# x64, where it belongs (arm64 uses the distro libicu74).
sed -i 's/^if  is_ubuntu24 ; then$/if is_ubuntu24_x64; then/' "$installers/configure-dpkg.sh"
# install-swift.sh imports nine PGP keys through ONE un-retried keyserver
# call; keyserver.ubuntu.com intermittently returns a subset, which aborts
# the multi-hour build at signature verification. Pre-import with retries.
swift_keys="'7463A81A4B2EEA1B551FFBCFD441C977412B37AD' '1BE1E29A084CB305F397D62A9F597F4D21A56D5F' 'A3BAFD3556A59079C06894BD63BC1CFE91D306C6' '5E4DF843FB065D7F7E24FBA2EF5430F071E1B235' '8513444E2DA36B7C1659AF4D7638F1FB2B2B08C4' 'A62AE125BBBFBB96A6E042EC925CC1CCED3D1561' '8A7495662C3CD4AE18D95637FAF6989E1BC16FEA' 'E813C892820A6FA13755B268F167DF1ACF9CE069' '52BB7E3DE28A71BE22EC05FFEF80A866B47A981F'"
for attempt in 1 2 3 4 5; do
    eval gpg --keyserver hkps://keyserver.ubuntu.com:443 --recv-keys $swift_keys && break
    echo "swift key import attempt $attempt failed; retrying" >&2
    sleep 15
done
gpg --list-keys EF80A866B47A981F >/dev/null 2>&1 || { echo 'swift signing key EF80A866B47A981F unobtainable' >&2; exit 1; }

# configure-environment.sh seds /etc/default/motd-news in place; the minimal
# base has none. Seed the upstream default so the disable edit applies.
[ -f /etc/default/motd-news ] || printf 'ENABLED=1\n' > /etc/default/motd-news
sed -i -e 's/^apt-get install /apt-get install -y /' -e 's/^apt-get dist-upgrade$/apt-get dist-upgrade -y/' "$installers/install-ms-repos.sh"

step 'apt sources and limits'
run_step bash -e "$installers/install-ms-repos.sh"
run_step bash -e "$installers/configure-apt-sources.sh"
run_step bash -e "$installers/configure-apt.sh"
run_step bash -e "$installers/configure-limits.sh"
run_step bash -e "$installers/configure-environment.sh"
run_step bash -e "$installers/install-apt-vital.sh"

step 'powershell (needed by the toolset installer itself)'
run_step bash -e "$installers/install-powershell.sh"
run_step pwsh -NoProfile -File "$installers/Install-PowerShellModules.ps1"
run_step pwsh -NoProfile -File "$installers/Install-PowerShellAzModules.ps1"

step 'language toolchains, CLIs, browsers, databases'
for script in \
    install-actions-cache.sh \
    install-apt-common.sh \
    install-azcopy.sh \
    install-azure-cli.sh \
    install-azure-devops-cli.sh \
    install-apache.sh \
    install-aws-tools.sh \
    install-clang.sh \
    install-swift.sh \
    install-cmake.sh \
    install-awf.sh \
    install-container-tools.sh \
    install-dotnetcore-sdk.sh \
    install-gcc-compilers.sh \
    install-firefox.sh \
    install-gfortran.sh \
    install-git.sh \
    install-git-lfs.sh \
    install-github-cli.sh \
    install-google-cloud-cli.sh \
    install-java-tools.sh \
    install-kubernetes-tools.sh \
    install-kotlin.sh \
    install-mysql.sh \
    install-nginx.sh \
    install-nvm.sh \
    install-nodejs.sh \
    install-copilot-cli.sh \
    install-bazel.sh \
    install-php.sh \
    install-postgresql.sh \
    install-pulumi.sh \
    install-ruby.sh \
    install-rust.sh \
    install-selenium.sh \
    install-packer.sh \
    install-vcpkg.sh \
    configure-dpkg.sh \
    install-yq.sh \
    install-python.sh \
    install-zstd.sh \
    install-ninja.sh; do
    run_step bash -e "$installers/$script"
done

# The guest root is btrfs; overlay2 on it cannot manage whiteouts
# (EIO "failed to register layer" during pulls) and loses native diff.
# Reset the docker.io-era graph and pin dockerd to the native btrfs driver
# before the upstream installer starts the new engine.
systemctl stop docker.socket docker.service 2>/dev/null || true
rm -rf /var/lib/docker
install -d /etc/docker
printf '{"storage-driver":"btrfs"}\n' > /etc/docker/daemon.json

step 'docker engine (docker-ce replaces the transitional docker.io package)'
run_step bash -e "$installers/install-docker.sh"

step 'pinned toolcache and docker plugins from toolset.json'
run_step pwsh -NoProfile -File "$installers/Install-Toolset.ps1"
run_step pwsh -NoProfile -File "$installers/Configure-Toolset.ps1"

step 'pipx packages'
run_step bash -e "$installers/install-pipx-packages.sh"

step 'homebrew (as the non-root runner user)'
su -s /bin/bash runner -c "cd /tmp && export HOME=/home/runner HELPER_SCRIPTS='$helpers' DEBIAN_FRONTEND=noninteractive && bash -e '$installers/install-homebrew.sh'"

step 'snap configuration (non-fatal: snapd is unavailable in some kernels)'
if ! run_step bash -e "$installers/configure-snap.sh"; then
    echo 'NOTE: configure-snap.sh failed; continuing without snap configuration' >&2
fi

step 'runner-account environment parity'
# Upstream expects the hosted runner account to be created AFTER the toolset
# (from /etc/skel) and post-generation to expand $HOME in /etc/environment;
# neither happens on a runner clone. Materialize both now:
#   1. skel -> the existing runner account (rustup/cargo, nvm, dotnet paths)
#   2. literal /home/runner in /etc/environment
#   3. /home/runner/actions-runner/.env with the non-PATH entries, because
#      GARM's JIT bootstrap only sources env.sh (PATH + a fixed key list) and
#      never re-reads /etc/environment, so setup-* actions would otherwise
#      miss AGENT_TOOLSDIRECTORY/RUNNER_TOOL_CACHE and use _work/_tool.
#      LIMITATION: when GARM requests a newer runner version than the template
#      cache, the provider deletes this directory so the upstream script
#      re-downloads the runner, and the seeded .env is not recreated on that
#      path (recreating the directory would suppress the download); setup-*
#      actions then fall back to _work/_tool until the template is rebuilt.
if [ -d /etc/skel ] && [ -n "$(ls -A /etc/skel 2>/dev/null)" ]; then
    cp -r /etc/skel/. /home/runner/
    chown -R runner:runner /home/runner
fi
sed -i 's|\$HOME|/home/runner|g' /etc/environment
install -d -m 0755 /home/runner/actions-runner
# The runner's .env is dotenv format: plain KEY=VALUE lines, no export prefix.
grep -v '^PATH=' /etc/environment | grep -E '^[A-Za-z_][A-Za-z0-9_]*=' > /home/runner/actions-runner/.env
chown runner:runner /home/runner/actions-runner/.env
chmod 0644 /home/runner/actions-runner/.env

step 'official image cleanup'
run_step bash -e "$installers/cleanup.sh"

step 'final system configuration'
# The minimal base has no needrestart; drop configure-system.sh's
# needrestart-only block rather than installing it (its apt hook restarts
# services during job package installs, which could kill a runner unit).
[ -f /etc/needrestart/needrestart.conf ] || sed -i '/^if is_ubuntu24; then$/,/^fi$/d' "$installers/configure-system.sh"
# configure-system.sh ends with "chmod -R 777 /opt", which crosses OrbStack's
# read-only /opt/orbstack-guest integration mount and aborts. Restrict the
# chmod to /opt's own filesystem.
sed -i 's#^chmod -R 777 /opt$#find /opt -path /opt/orbstack-guest -prune -o -exec chmod 777 {} +#' "$installers/configure-system.sh"
# install-docker.sh changes the docker GID (groupmod) while the apt-started
# daemon is running with the old group; its un-retried conditional start then
# fails in OrbStack guests ("Job for docker.service failed"). Restart the
# daemon after the GID change, with retries.
sed -i 's#systemctl is-active --quiet docker.service || systemctl start docker.service#for _gd in $(seq 1 5); do systemctl restart docker.service \&\& break; echo "docker restart retry ${_gd}" >\&2; sleep 10; done#' "$installers/install-docker.sh"
run_step bash -e "$installers/configure-system.sh"

rm -rf "$work"
printf '\n===== toolset installation complete =====\n' >&2
