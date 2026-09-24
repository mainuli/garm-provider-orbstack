package main

import (
	"context"
	"errors"
	"flag"

	"github.com/mainuli/garm-provider-orbstack/internal/install"
)

func runUninstall(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	path := flags.String("config", "", "absolute managed host.toml path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("uninstall accepts flags only")
	}
	return install.Uninstall(ctx, *path)
}
