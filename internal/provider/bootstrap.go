package provider

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/cloudbase/garm-provider-common/cloudconfig"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/cloudbase/garm-provider-common/util"
	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
	"github.com/mainuli/garm-provider-orbstack/internal/templates"
)

var scriptNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var packagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*(?::[a-z0-9][a-z0-9-]*)?(?:=[A-Za-z0-9.+:~_-]+)?$`)

type guestFile struct {
	name string
	data []byte
	mode int64
}

type bootstrapPlan struct {
	files       []guestFile
	removeCache bool
}

func cacheMatches(manifest templates.Manifest, tools params.RunnerApplicationDownload) bool {
	return manifest.RunnerFilename == tools.GetFilename() &&
		(tools.GetSHA256Checksum() == "" || strings.EqualFold(manifest.RunnerSHA256, tools.GetSHA256Checksum()))
}

func prepareBootstrap(bootstrap params.BootstrapInstance, manifest templates.Manifest) (bootstrapPlan, error) {
	if len(bootstrap.SSHKeys) != 0 {
		return bootstrapPlan{}, errors.New("ssh-keys are unsupported: runners have no guest SSH service")
	}
	var specs cloudconfig.CloudConfigSpec
	if len(bootstrap.ExtraSpecs) != 0 {
		if bytes.Equal(bytes.TrimSpace(bootstrap.ExtraSpecs), []byte("null")) {
			return bootstrapPlan{}, errors.New("extra_specs must be an object")
		}
		decoder := json.NewDecoder(bytes.NewReader(bootstrap.ExtraSpecs))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&specs); err != nil {
			return bootstrapPlan{}, errors.New("invalid extra_specs: only runner_install_template, extra_context and pre_install_scripts are supported")
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return bootstrapPlan{}, errors.New("extra_specs contains trailing data")
		}
	}
	for name := range specs.PreInstallScripts {
		if !scriptNamePattern.MatchString(name) {
			return bootstrapPlan{}, errors.New("pre-install scripts require safe single-component filenames")
		}
	}
	for _, pkg := range bootstrap.UserDataOptions.ExtraPackages {
		if !packagePattern.MatchString(pkg) {
			return bootstrapPlan{}, errors.New("extra_packages contains an invalid Debian package specification")
		}
	}
	tools, err := util.GetTools(params.Linux, bootstrap.OSArch, bootstrap.Tools)
	if err != nil {
		return bootstrapPlan{}, errors.New("no compatible Linux runner tools supplied")
	}
	if checksum := tools.GetSHA256Checksum(); checksum != "" && !templates.ValidSHA256(checksum) {
		return bootstrapPlan{}, errors.New("invalid runner tools SHA-256")
	}
	script, err := cloudconfig.GetRunnerInstallScript(bootstrap, tools, bootstrap.Name)
	if err != nil {
		// Template expansion errors can contain the caller's template or tokens.
		return bootstrapPlan{}, errors.New("unable to render GARM runner-install template")
	}
	plan := bootstrapPlan{removeCache: !cacheMatches(manifest, tools)}
	plan.files = append(plan.files, guestFile{"runner-install.sh", script, 0700})
	var prep strings.Builder
	prep.WriteString("#!/bin/sh\nset -eu\numask 077\ntrap 'rm -rf /run/garm' EXIT\n")
	bundle := bytes.TrimSpace(bootstrap.CACertBundle)
	certCount := 0
	for len(bundle) != 0 {
		if !bytes.HasPrefix(bundle, []byte("-----BEGIN CERTIFICATE-----")) {
			return bootstrapPlan{}, errors.New("invalid controller CA bundle")
		}
		block, rest := pem.Decode(bundle)
		if block == nil || block.Type != "CERTIFICATE" {
			return bootstrapPlan{}, errors.New("invalid controller CA bundle")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return bootstrapPlan{}, errors.New("invalid controller CA certificate")
		}
		certName := fmt.Sprintf("ca-%04d.crt", certCount)
		plan.files = append(plan.files, guestFile{certName, pem.EncodeToMemory(block), 0600})
		fmt.Fprintf(&prep, "install -m 0644 /run/garm/%s /usr/local/share/ca-certificates/garm-%04d.crt\n", certName, certCount)
		certCount++
		bundle = bytes.TrimSpace(rest)
	}
	if certCount != 0 {
		prep.WriteString("update-ca-certificates\n")
	}
	if len(bootstrap.UserDataOptions.ExtraPackages) != 0 {
		// This runs asynchronously in the guest, never during Create. Refresh
		// apt's index only for explicitly requested packages; never upgrade.
		prep.WriteString("export DEBIAN_FRONTEND=noninteractive\napt-get -o DPkg::Lock::Timeout=600 update\napt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends --")
		for _, pkg := range bootstrap.UserDataOptions.ExtraPackages {
			fmt.Fprintf(&prep, " '%s'", pkg)
		}
		prep.WriteByte('\n')
	}
	names := make([]string, 0, len(specs.PreInstallScripts))
	for name := range specs.PreInstallScripts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		plan.files = append(plan.files, guestFile{"pre-" + name, specs.PreInstallScripts[name], 0700})
		fmt.Fprintf(&prep, "/run/garm/pre-%s\n", name)
	}
	prep.WriteString("su -l -c /run/garm/runner-install.sh runner\n")
	plan.files = append(plan.files, guestFile{"prepare.sh", []byte(prep.String()), 0700})
	plan.files = append(plan.files, guestFile{"garm-bootstrap.service", []byte(bootstrapUnit), 0600})
	return plan, nil
}

const bootstrapUnit = `[Unit]
Description=GARM ephemeral runner bootstrap
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/bin/sh /run/garm/prepare.sh
ExecStopPost=/usr/bin/rm -rf /run/garm
TimeoutStartSec=infinity
StandardOutput=append:/var/log/garm-bootstrap.log
StandardError=append:/var/log/garm-bootstrap.log
UMask=0077
`

// injectBootstrap transfers secrets only over stdin. Output stays in the guest;
// upstream templates may contain set -x and bearer-token curl arguments there.
func (p *Provider) injectBootstrap(ctx context.Context, machineID string, plan bootstrapPlan) error {
	if plan.removeCache {
		if err := p.guest(ctx, machineID, []string{"rm", "-rf", "/home/runner/actions-runner"}, nil); err != nil {
			return err
		}
		// Full-variant templates carry the hosted-toolcache environment in
		// /etc/garm-template/runner.env and materialize it as the runner's
		// .env (GARM's JIT bootstrap only sources PATH and a fixed key
		// list, so setup-* actions need it to find /opt/hostedtoolcache).
		// The directory removal above deleted that .env; rebuild it.
		if err := p.guest(ctx, machineID, []string{"sh", "-c",
			"if [ -f /etc/garm-template/runner.env ]; then install -d -m 0755 -o runner -g runner /home/runner/actions-runner && sed 's|^|export |' /etc/garm-template/runner.env > /home/runner/actions-runner/.env && chown runner:runner /home/runner/actions-runner/.env && chmod 0644 /home/runner/actions-runner/.env; fi"}, nil); err != nil {
			return err
		}
	}
	if err := p.guest(ctx, machineID, []string{"install", "-d", "-m", "0711", "-o", "root", "-g", "root", "/run/garm"}, nil); err != nil {
		return err
	}
	var payload bytes.Buffer
	tw := tar.NewWriter(&payload)
	for _, file := range plan.files {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(file.data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := p.guest(ctx, machineID, []string{"tar", "-xf", "-", "-C", "/run/garm", "--no-same-owner"}, &payload); err != nil {
		return err
	}
	if err := p.guest(ctx, machineID, []string{"chown", "runner:runner", "/run/garm/runner-install.sh"}, nil); err != nil {
		return err
	}
	if err := p.guest(ctx, machineID, []string{"install", "-m", "0644", "/run/garm/garm-bootstrap.service", "/etc/systemd/system/garm-bootstrap.service"}, nil); err != nil {
		return err
	}
	if err := p.guest(ctx, machineID, []string{"systemctl", "daemon-reload"}, nil); err != nil {
		return err
	}
	return p.guest(ctx, machineID, []string{"systemctl", "start", "--no-block", "garm-bootstrap.service"}, nil)
}

func (p *Provider) guest(ctx context.Context, id string, argv []string, stdin io.Reader) error {
	if err := p.orb.Run(ctx, orbstack.RunOptions{Machine: id, User: "root", Command: argv, Stdin: stdin, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("guest bootstrap injection: %w", ctx.Err())
		}
		// Do not expose guest stderr, templates or arbitrary payload data.
		return errors.New("guest bootstrap injection failed; inspect the disposable machine locally")
	}
	return nil
}
