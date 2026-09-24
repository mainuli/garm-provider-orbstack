package orbstack

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestOrbctlEnvProvidesHOME reproduces the GARM-stripped environment: without
// the adapter's env reconstruction orbctl panics ($HOME is not defined); with
// orbctlEnv() it works. Skips when orbctl is unavailable.
func TestOrbctlEnvProvidesHOME(t *testing.T) {
	orbctl, err := exec.LookPath("orbctl")
	if err != nil {
		t.Skip("orbctl not available")
	}
	// Reproduce the stripped environment failure mode.
	stripped := []string{"GARM_COMMAND=ListInstances"}
	cmd := exec.Command(orbctl, "version")
	cmd.Env = stripped
	if err := cmd.Run(); err == nil {
		t.Fatal("expected orbctl to fail without HOME (environment not actually stripped?)")
	}
	// The adapter's env must restore HOME from the user database even when
	// the current process has none.
	env := append([]string{"GARM_COMMAND=ListInstances"}, orbctlEnv()...)
	sawHome := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") && len(kv) > len("HOME=") {
			sawHome = true
		}
	}
	if !sawHome {
		t.Skip("no HOME recoverable in this environment (user.Current failed)")
	}
	cmd = exec.Command(orbctl, "version")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "Version:") {
		t.Fatalf("orbctl with reconstructed env failed: %v %q", err, out)
	}
	_ = os.Environ
}
