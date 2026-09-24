package main

import (
	"context"
	"errors"
	"flag"

	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/provider"
)

func runRecover(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("recover", flag.ContinueOnError)
	name := flags.String("runner-name", "", "original GARM runner name to recover")
	machineID := flags.String("machine-id", "", "inspected OrbStack machine ID to adopt")
	release := flags.Bool("release", false, "release an absent machine-less reservation after inspection")
	path := flags.String("config", "", "host.toml (defaults to the installed host configuration)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *name == "" || (*machineID == "") == !*release {
		return errors.New("recover requires --runner-name and exactly one of --machine-id or --release")
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
	instanceProvider, err := provider.New(ctx, host, host.ControllerID, "")
	if err != nil {
		return err
	}
	return instanceProvider.Recover(ctx, *name, *machineID, *release)
}
