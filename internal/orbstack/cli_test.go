package orbstack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOrbctl writes a fake orbctl script that simulates OrbStack behavior
// around quota-stuck machines:
//   - rename fails with ENOSPC unless a `config set machine.<current>.disk_bytes 0`
//     already appears in the call log (a machine at its quota cannot be renamed)
//   - delete succeeds only after a successful rename updated the state file
//
// cfgFail makes every `config` invocation exit 1 (simulates an orbctl that
// rejects the key); renames then still succeed, modeling a healthy machine
// below quota.
func fakeOrbctl(t *testing.T, dir string, cfgFail bool) string {
	t.Helper()
	log := filepath.Join(dir, "calls.log")
	state := filepath.Join(dir, "name")
	orbctl := filepath.Join(dir, "orbctl")
	cfg := "exit 0"
	if cfgFail {
		cfg = "exit 1"
	}
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + log + "\n" +
		"case \"$1\" in\n" +
		"  list)\n" +
		"    NAME=$(cat " + state + " 2>/dev/null || echo old)\n" +
		"    printf '[{\"id\":\"01TEST\",\"name\":\"%s\",\"state\":\"stopped\"}]' \"$NAME\"\n" +
		"    ;;\n" +
		"  info)\n" +
		"    NAME=$(cat " + state + " 2>/dev/null || echo old)\n" +
		"    printf '{\"record\":{\"id\":\"01TEST\",\"name\":\"%s\"}}' \"$NAME\"\n" +
		"    ;;\n" +
		"  config)\n" +
		"    " + cfg + "\n" +
		"    ;;\n" +
		"  rename)\n" +
		"    if grep -q \"config set machine.old.disk_bytes 0\" " + log + " 2>/dev/null || [ " + boolStr(cfgFail) + " = yes ]; then\n" +
		"      echo \"$3\" > " + state + "\n" +
		"      exit 0\n" +
		"    else\n" +
		"      echo \"no space left on device\" >&2\n" +
		"      exit 1\n" +
		"    fi\n" +
		"    ;;\n" +
		"  delete)\n" +
		"    if [ -s " + state + " ]; then\n" +
		"      exit 0\n" +
		"    else\n" +
		"      echo \"no such machine\" >&2\n" +
		"      exit 1\n" +
		"    fi\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(orbctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// callIndex returns the log line index of the first line containing all the
// given substrings, or -1.
func callIndex(t *testing.T, log string, want ...string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("reading call log: %v", err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		match := true
		for _, w := range want {
			if !strings.Contains(line, w) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// TestDeleteLiftsQuotaBeforeRename proves the provider lifts the disk quota
// on the machine's current (pre-rename) name before renaming: a machine at
// its disk_bytes quota cannot be renamed at all (btrfs ENOSPC), so a lift
// placed after the rename never runs. The fake rejects rename unless
// `config set machine.old.disk_bytes 0` precedes it.
func TestDeleteLiftsQuotaBeforeRename(t *testing.T) {
	dir := t.TempDir()
	log := fakeOrbctl(t, dir, false)
	c := New(filepath.Join(dir, "orbctl"))

	if err := c.Delete(context.Background(), "01TEST"); err != nil {
		t.Fatalf("Delete should succeed when the quota is lifted first: %v", err)
	}
	lift := callIndex(t, log, "config", "set", "machine.old.disk_bytes", "0")
	rename := callIndex(t, log, "rename")
	del := callIndex(t, log, "delete", "--force")
	if lift == -1 {
		t.Fatal("Delete must set disk_bytes 0 on the pre-rename name before renaming")
	}
	if rename == -1 || del == -1 {
		t.Fatal("Delete must rename and delete")
	}
	if lift > rename {
		t.Fatalf("quota lift (line %d) must precede rename (line %d)", lift, rename)
	}
	if rename > del {
		t.Fatalf("rename (line %d) must precede delete (line %d)", rename, del)
	}
}

// TestDeleteCapLiftIsBestEffort proves a failing config-set does not block
// deletion of a healthy machine: the delete path must still attempt rename
// and delete. (Regression guard: a hard failure here would break every
// teardown whenever orbctl rejects or errors on the config key.)
func TestDeleteCapLiftIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	log := fakeOrbctl(t, dir, true)
	c := New(filepath.Join(dir, "orbctl"))

	if err := c.Delete(context.Background(), "01TEST"); err != nil {
		t.Fatalf("Delete of a healthy machine must succeed despite config-set failure: %v", err)
	}
	if callIndex(t, log, "config", "set") == -1 {
		t.Fatal("Delete must still attempt the quota lift")
	}
	if callIndex(t, log, "delete", "--force") == -1 {
		t.Fatal("Delete must still run delete --force after a failed lift")
	}
}
