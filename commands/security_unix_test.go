//go:build !windows

package commands

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// MED3: /dev/zero (via a symlink) read 1.26 GB; a FIFO or device must be
// refused without reading, and a regular file is read bounded.
func TestPushRefusesNonRegularEntries(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	h.run("init", "--source", "src-1")
	if err := os.Symlink("/dev/zero", filepath.Join(h.env.Cwd, "schema.dbml")); err != nil {
		t.Fatal(err)
	}
	if code := h.run("push"); code != 1 {
		t.Fatalf("/dev/zero: %d", code)
	}
	_ = os.Remove(filepath.Join(h.env.Cwd, "schema.dbml"))
	if err := syscall.Mkfifo(filepath.Join(h.env.Cwd, "schema.dbml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := h.run("push"); code != 1 || !strings.Contains(h.stderr.String(), "not a regular file") {
		t.Fatalf("fifo: %d %s", code, h.stderr)
	}
	if h.api.requests != 0 {
		t.Fatal("request sent")
	}
}
