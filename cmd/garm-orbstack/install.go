package main

import (
	"context"
	"errors"
	"flag"

	"github.com/mainuli/garm-provider-orbstack/internal/install"
)

func runInstall(ctx context.Context, args []string) error {
	if err := install.ValidateReleaseMetadata(); err != nil {
		return err
	}
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	var opts install.Options
	flags.StringVar(&opts.ConfigPath, "config", "", "absolute managed host.toml path (default: ~/.config/garm-orbstack/host.toml)")
	flags.StringVar(&opts.FromDir, "from-dir", "", "checksummed release staging directory")
	flags.BoolVar(&opts.ConfirmVersionChange, "confirm-version-change", false, "authorize a stopped-controller backup and deliberate version change")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("install accepts flags only")
	}
	return install.Install(ctx, opts)
}
