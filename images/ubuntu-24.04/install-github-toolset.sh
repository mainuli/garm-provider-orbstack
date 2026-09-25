#!/bin/bash
# Applies the pinned official actions/runner-images Ubuntu 24.04 toolset to a
# NEW template build (--variant full). Runs only at build time, as root, inside
# the template machine; never on runner clones. The result matches the software
# set of GitHub's hosted ubuntu-24.04 arm64 image.
#
# Deviations from the official packer sequence (each Azure-image-only):
#   - no configure-apt-mock.sh, no waagent deprovision, no imagedata for Azure
#   - no SoftwareReport/RunAll-Tests suite (the builder captures dpkg output
#     into the manifest instead)
#   - no reboot between install and cleanup (OrbStack machines reboot is
#     unnecessary for this flow)
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

repo="$work/repo/images/ubuntu"
helpers=/image-generation/helpers
installers=/image-generation/installers
image_folder=/image-generation
mkdir -p "$helpers" "$installers" "$image_folder"
cp -r "$repo/scripts/helpers/." "$helpers/"
cp -r "$repo/scripts/build/." "$installers/"
cp "$repo/toolsets/toolset-2404-arm64.json" "$installers/toolset.json"

export HELPER_SCRIPTS="$helpers"
export INSTALLER_SCRIPT_FOLDER="$installers"
export IMAGE_FOLDER="$image_folder"
export IMAGE_OS=ubuntu24
export IMAGE_VERSION="$tag"
export ARCHITECTURE=arm64
export DEBIAN_FRONTEND=noninteractive

# Minimal imagedata so official scripts that read it succeed without the Azure
# agent machinery.
printf 'IMAGE_VERSION=%s\nIMAGE_OS=ubuntu24\n' "$tag" > "$image_folder/imagedata.json"

step() { printf '\n===== toolset step: %s =====\n' "$*" >&2; }

step 'apt sources and limits'
bash "$installers/install-ms-repos.sh"
bash "$installers/configure-apt-sources.sh"
bash "$installers/configure-apt.sh"
bash "$installers/configure-limits.sh"
bash "$installers/configure-environment.sh"
bash "$installers/install-apt-vital.sh"

step 'powershell (needed by the toolset installer itself)'
bash "$installers/install-powershell.sh"
pwsh -NoProfile -File "$installers/Install-PowerShellModules.ps1"
pwsh -NoProfile -File "$installers/Install-PowerShellAzModules.ps1"

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
    bash "$installers/$script"
done

step 'docker engine (docker-ce replaces the transitional docker.io package)'
bash "$installers/install-docker.sh"

step 'pinned toolcache and docker plugins from toolset.json'
pwsh -NoProfile -File "$installers/Install-Toolset.ps1"
pwsh -NoProfile -File "$installers/Configure-Toolset.ps1"

step 'pipx packages'
bash "$installers/install-pipx-packages.sh"

step 'homebrew (as the non-root runner user)'
su -s /bin/bash runner -c "cd /tmp && export HOME=/home/runner HELPER_SCRIPTS='$helpers' DEBIAN_FRONTEND=noninteractive && bash '$installers/install-homebrew.sh'"

step 'snap configuration (non-fatal: snapd is unavailable in some kernels)'
if ! bash "$installers/configure-snap.sh"; then
    echo 'NOTE: configure-snap.sh failed; continuing without snap configuration' >&2
fi

step 'official image cleanup'
bash "$installers/cleanup.sh"

step 'final system configuration'
bash "$installers/configure-system.sh"

rm -rf "$work"
printf '\n===== toolset installation complete =====\n' >&2
