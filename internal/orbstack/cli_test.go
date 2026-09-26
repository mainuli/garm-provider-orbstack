package orbstack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeleteRaisesCapBeforeForceDelete proves the provider lifts the disk
// quota before the destructive delete: a machine at its disk_bytes quota
// cannot be cleaned up by btrfs (ENOSPC during subvolume deletion), so
// Delete must raise the cap between the rename and the delete.
func TestDeleteRaisesCapBeforeForceDelete(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	state := filepath.Join(dir, "name")
	orbctl := filepath.Join(dir, "orbctl")

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
		"  rename)\n" +
		"    echo \"$3\" > " + state + "\n" +
		"    ;;\n" +
		"  config)\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"  delete)\n" +
		"    if grep -q \"config.*set.*disk_bytes 0\" " + log + " 2>/dev/null; then\n" +
		"      exit 0\n" +
		"    else\n" +
		"      echo \"delete called before cap was raised\" >&2\n" +
		"      exit 1\n" +
		"    fi\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"

	if err := os.WriteFile(orbctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(orbctl)
	if err := c.Delete(context.Background(), "01TEST"); err != nil {
		t.Fatalf("Delete should succeed when cap is raised first: %v", err)
	}
	raw, _ := os.ReadFile(log)
	lines := strings.Split(string(raw), "\n")
	configIdx, deleteIdx := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "config") && strings.Contains(line, "disk_bytes") && strings.HasSuffix(strings.TrimSpace(line), "0") {
			configIdx = i
		}
		if strings.Contains(line, "delete") {
			deleteIdx = i
		}
	}
	if configIdx == -1 {
		t.Fatal("Delete must set disk_bytes before deleting")
	}
	if deleteIdx == -1 {
		t.Fatal("Delete must call delete")
	}
	if configIdx > deleteIdx {
		t.Fatalf("config set (line %d) must precede delete (line %d)", configIdx, deleteIdx)
	}
}
