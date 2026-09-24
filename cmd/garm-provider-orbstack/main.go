// Command garm-provider-orbstack is a native macOS GARM external provider.
// GARM supplies the v0.1.0 SDK environment and bootstrap JSON on stdin.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloudbase/garm-provider-common/execution"
	"github.com/cloudbase/garm-provider-common/execution/common"
	executionv010 "github.com/cloudbase/garm-provider-common/execution/v0.1.0"
	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/provider"
)

func run(ctx context.Context) error {
	// Reject v0.1.1 before its parser can accept commands outside our contract.
	if version := os.Getenv("GARM_INTERFACE_VERSION"); version != "" && version != common.Version010 {
		return errors.New("only external-provider interface v0.1.0 is supported")
	}
	parsed, err := execution.GetEnvironment()
	if err != nil {
		return err
	}
	environment := executionv010.EnvironmentV010{
		Command: parsed.EnvironmentV010.Command, ControllerID: parsed.ControllerID,
		PoolID: parsed.EnvironmentV010.PoolID, ProviderConfigFile: parsed.ProviderConfigFile,
		InstanceID: parsed.EnvironmentV010.InstanceID, BootstrapParams: parsed.EnvironmentV010.BootstrapParams,
	}
	if err := environment.Validate(); err != nil {
		return err
	}
	host, err := config.LoadHost(environment.ProviderConfigFile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, host.OperationTimeout)
	defer cancel()
	instanceProvider, err := provider.New(ctx, host, environment.ControllerID, environment.PoolID)
	if err != nil {
		return err
	}
	result, err := environment.Run(ctx, instanceProvider)
	if err != nil {
		return err
	}
	if result != "" {
		_, err = fmt.Fprint(os.Stdout, result)
	}
	return err
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(common.ResolveErrorToExitCode(err))
}
