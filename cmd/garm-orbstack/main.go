// Command garm-orbstack is the host-side helper for the GARM OrbStack
// integration: installer, doctor, template builder, recovery tool, scoped
// garm-cli wrapper and uninstaller. The GARM controller executes its
// companion binary (cmd/garm-provider-orbstack) directly as an external
// provider; this CLI is for the operator.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mainuli/garm-provider-orbstack/internal/releaseinfo"
)

// exitCodeError lets a subcommand request a specific process exit code
// (doctor uses 2 for "healthy but no template registered").
type exitCodeError struct {
	code int
	err  error
}

func (e exitCodeError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit code %d", e.code)
	}
	return e.err.Error()
}

func (e exitCodeError) ExitCode() int { return e.code }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	var err error
	cmd, rest := os.Args[1], os.Args[2:]
	switch cmd {
	case "version":
		err = runVersion(rest)
	case "install":
		err = runInstall(ctx, rest)
	case "doctor":
		err = runDoctor(ctx, rest)
	case "template":
		if len(rest) == 0 {
			usage(os.Stderr)
			os.Exit(2)
		}
		switch sub, subRest := rest[0], rest[1:]; sub {
		case "build":
			err = runTemplateBuild(ctx, subRest)
		case "list":
			err = runTemplateList(ctx, subRest)
		default:
			usage(os.Stderr)
			os.Exit(2)
		}
	case "recover":
		err = runRecover(ctx, rest)
	case "garm-cli":
		err = runGarmCli(ctx, rest)
	case "uninstall":
		err = runUninstall(ctx, rest)
	case "-h", "--help", "help":
		usage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "garm-orbstack: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		if asEC, ok := err.(interface{ ExitCode() int }); ok {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(asEC.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "garm-orbstack: %v\n", err)
		os.Exit(1)
	}
}

func runVersion(_ []string) error {
	// Output is a machine-readable contract: install.sh and the installer's
	// provenance check compare the entire trimmed line to the release tag.
	// Provenance details (GARM source) belong to doctor, not here.
	fmt.Println(releaseinfo.Version)
	return nil
}

func usage(w *os.File) {
	fmt.Fprint(w, `Usage: garm-orbstack <command> [flags]

Commands:
  version                 Print version (and bundled GARM source when set)
  install                 Install/verify the controller service and provider
  doctor                  Check installation health (exit 2: no template)
  template build          Build and register a runner template machine
  template list           List registered template images
  recover                 Adopt or release a pending reservation
  garm-cli <args...>      Scoped garm-cli wrapper
  uninstall               Remove the managed installation (preserves data)

Run `+"`garm-orbstack <command> --help`"+` for command flags.
`)
}
