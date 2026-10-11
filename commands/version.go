package commands

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// resolveVersion prefers the -ldflags version, then the module version a
// `go install …@vX` build records.
func resolveVersion(v string) string {
	if v != "" && v != "dev" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func runVersion(env *Env) error {
	fmt.Fprintf(env.Stdout, "dbdiagram %s (%s %s/%s)\n", resolveVersion(env.Version), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}
