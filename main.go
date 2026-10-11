// Command dbdiagram syncs database schemas with dbdiagram (appleby.cloud):
// push DBML / SQL / schema.rb or a live database inspection to a diagram
// source, and pull a diagram's DBML and layout back to disk.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/aidenappl/dbdiagram-cli/commands"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := commands.Run(ctx, os.Args[1:], commands.DefaultEnv(version))
	stop()
	os.Exit(code)
}
