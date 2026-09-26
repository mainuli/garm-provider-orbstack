package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/templates"
)

func templateConfigPath(path string) (string, error) {
	if path == "" {
		return config.DefaultHostPath()
	}
	// The plan has a single canonical host config per owner. Resolve and
	// enforce that BEFORE any OrbStack work: a relative or stray path must
	// fail here, not after an hour of provisioning an unregisterable
	// template.
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	defaultPath, err := config.DefaultHostPath()
	if err != nil {
		return "", err
	}
	if filepath.Clean(resolved) != filepath.Clean(defaultPath) {
		return "", fmt.Errorf("--config must name the single managed host configuration %s", defaultPath)
	}
	return resolved, nil
}

func runTemplateBuild(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("template build", flag.ContinueOnError)
	var options templates.BuildOptions
	flags.StringVar(&options.Arch, "arch", "", "runner architecture: arm64 or amd64 (emulated on Apple Silicon)")
	flags.StringVar(&options.RunnerVersion, "runner-version", "", "pinned actions/runner release version, without v")
	flags.StringVar(&options.RunnerSHA256, "runner-sha256", "", "verified SHA-256 of the official runner archive")
	flags.StringVar(&options.Variant, "variant", "minimal", "software scope: minimal, or full for the pinned official actions/runner-images Ubuntu 24.04 toolset (arm64; builds take hours)")
	path := flags.String("config", "", "host.toml (defaults to the installed host configuration)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("template build accepts flags only")
	}
	if err := options.Validate(); err != nil {
		return err
	}
	resolved, err := templateConfigPath(*path)
	if err != nil {
		return err
	}
	// Image creation includes package installation and archive verification;
	// unlike per-runner bootstrap it is an explicit long-running operation.
	// The minimal recipe fits a one-hour ceiling; the full runner-images
	// toolset legitimately takes hours, so it runs on the signal context
	// alone (cancellable, never silently killed mid-install).
	if options.Variant != "full" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Hour)
		defer cancel()
	}
	manifest, err := templates.Build(ctx, resolved, options)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(manifest)
}

func runTemplateList(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("template list", flag.ContinueOnError)
	path := flags.String("config", "", "host.toml (defaults to the installed host configuration)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("template list accepts flags only")
	}
	resolved, err := templateConfigPath(*path)
	if err != nil {
		return err
	}
	host, err := config.LoadHost(resolved)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, host.OperationTimeout)
	defer cancel()
	manifests, err := templates.List(ctx, host)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(manifests)
}

func runTemplateRemove(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("template remove", flag.ContinueOnError)
	path := flags.String("config", "", "host.toml (defaults to the installed host configuration)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("template remove takes exactly one image ID")
	}
	resolved, err := templateConfigPath(*path)
	if err != nil {
		return err
	}
	if err := templates.Remove(ctx, resolved, flags.Arg(0)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "template %s removed\n", flags.Arg(0))
	return nil
}
