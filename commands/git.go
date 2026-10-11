package commands

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// gitOutput runs a read-only git query in dir and returns its trimmed
// output, or "" when git is missing or dir is not a repository.
func gitOutput(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
