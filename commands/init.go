package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aidenappl/dbdiagram-cli/config"
)

func runInit(args []string, env *Env) error {
	fs := newFlagSet("init", "init --source <sourceID> [--entry schema.dbml] [--format dbml|mysql|postgres|mssql|schemarb] [--force]", env)
	source := fs.String("source", "", "source id (from the diagram's Sources panel)")
	entry := fs.String("entry", "schema.dbml", "entry file, relative to this directory")
	format := fs.String("format", "", "entry format (default: dbml, or schemarb for .rb)")
	force := fs.Bool("force", false, "overwrite an existing .dbdiagram/settings.json")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *source == "" {
		fmt.Fprintln(env.Stderr, "--source is required")
		fs.Usage()
		return errUsage
	}
	f := *format
	if f == "" {
		f = "dbml"
		if strings.EqualFold(filepath.Ext(*entry), ".rb") {
			f = "schemarb"
		}
		if strings.EqualFold(filepath.Ext(*entry), ".sql") {
			return errors.New("an .sql entry needs --format mysql, postgres or mssql")
		}
	}
	s := config.Settings{SourceID: *source, Entry: filepath.ToSlash(*entry), Format: f}
	if err := s.Validate(); err != nil {
		return err
	}
	path := filepath.Join(env.Cwd, config.DIR_NAME, config.SETTINGS_FILE)
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s already exists (pass --force to overwrite)", filepath.Join(config.DIR_NAME, config.SETTINGS_FILE))
	}
	if err := config.SaveSettings(env.Cwd, s); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Wrote %s (source %s, entry %q, format %s).\n",
		filepath.Join(config.DIR_NAME, config.SETTINGS_FILE), s.SourceID, s.Entry, s.Format)
	fmt.Fprintln(env.Stdout, "Commit it; .dbdiagram/state.json is local and gitignored.")
	return nil
}
