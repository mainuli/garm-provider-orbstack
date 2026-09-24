package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/templates"
)

func templateConfigPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return config.DefaultHostPath()
}

func runTemplateBuild(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("template build", flag.ContinueOnError)
	var options templates.BuildOptions
	flags.StringVar(&options.Arch, "arch", "", "runner architecture: arm64 or amd64 (emulated on Apple Silicon)")
	flags.StringVar(&options.RunnerVersion, "runner-version", "", "pinned actions/runner release version, without v")
	flags.StringVar(&options.RunnerSHA256, "runner-sha256", "", "verified SHA-256 of the official runner archive")
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
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
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
