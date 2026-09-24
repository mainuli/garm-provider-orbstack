package main

import (
	"context"

	"github.com/mainuli/garm-provider-orbstack/internal/install"
)

func runGarmCli(ctx context.Context, args []string) error {
	return install.GarmCLI(ctx, args)
}
