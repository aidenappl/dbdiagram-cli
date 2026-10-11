package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/aidenappl/dbdiagram-cli/config"
)

const pullUsage = `pull [--force] [--out FILE] [--source ID] [--api URL] [--verbose]

Writes the diagram's DBML to the entry file and its layout (table positions,
groups, notes) to the companion <entry>.dbdiagram file. Refuses to overwrite
an entry file changed since the last pull or push unless --force.`

// ErrLocalChanges is returned when pull would overwrite unpushed edits.
var ErrLocalChanges = errors.New("local changes")

func runPull(ctx context.Context, args []string, env *Env) error {
	fset := newFlagSet("pull", pullUsage, env)
	var a apiFlags
	a.register(fset)
	force := fset.Bool("force", false, "overwrite local changes")
	out := fset.String("out", "", "write the DBML here instead of the entry file (required when the entry format is not dbml)")
	source := fset.String("source", "", "source id (overrides settings)")
	if err := parse(fset, args); err != nil {
		return err
	}
	root, settings, err := projectRoot(env)
	if err != nil && !errors.Is(err, config.ErrNoSettings) {
		return err
	}
	if settings == nil {
		if *source == "" || *out == "" {
			return err // ErrNoSettings: needs init, or --source + --out
		}
		settings, root = &config.Settings{Format: "dbml"}, env.Cwd
	}
	sourceID := firstNonEmpty(*source, settings.SourceID)
	if !config.ValidSourceID(sourceID) {
		return fmt.Errorf("source id %q is not valid", sourceID)
	}

	// dir/name: the entry is written through a root on the project; an
	// explicit --out (the only way to write outside it) through a root on
	// its own directory. Neither is ever written through a final symlink.
	if *out == "-" {
		// DBML to stdout only: no layout file, no state, no local check.
		c, err := newClient(env, a)
		if err != nil {
			return err
		}
		res, err := c.Pull(ctx, sourceID)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(env.Stdout, res.Diagram.DBML)
		return err
	}
	var dir, name, rel string
	switch {
	case *out != "":
		if dir, name, err = config.OutTarget(env.Cwd, *out); err != nil {
			return err
		}
		abs := filepath.Join(dir, name)
		rel = filepath.ToSlash(abs)
		if r, ok := config.RelInside(root, abs); ok {
			rel = r
		}
	case settings.Format != "dbml":
		return fmt.Errorf("the entry %q is %s, and the diagram is DBML: pass --out FILE.dbml", settings.Entry, settings.Format)
	default:
		dir, name, rel = root, settings.Entry, settings.Entry
	}
	proj, err := config.OpenProject(dir)
	if err != nil {
		return err
	}
	defer proj.Close()

	c, err := newClient(env, a)
	if err != nil {
		return err
	}
	res, err := c.Pull(ctx, sourceID)
	if err != nil {
		return err
	}
	remote := []byte(res.Diagram.DBML)

	state, err := config.LoadState(root)
	if err != nil {
		return err
	}
	if !*force {
		if err := checkClean(proj, name, rel, sourceID, remote, state); err != nil {
			return err
		}
	}
	if err := proj.WriteFile(name, remote, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", rel, err)
	}
	if err := proj.WriteFile(config.LayoutPath(name), layoutDocument(res.Layout), 0o644); err != nil {
		return fmt.Errorf("write %q: %w", config.LayoutPath(rel), err)
	}
	if err := config.SaveState(root, config.State{
		SourceID: sourceID, Version: res.Diagram.Version, File: rel,
		ContentSHA256: config.HashContent(remote), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Pulled %q version %d → %q (layout → %q).\n",
		res.Diagram.Name, res.Diagram.Version, rel, config.LayoutPath(rel))
	if res.Source != nil && res.Source.Status == "paused" {
		fmt.Fprintln(env.Stdout, "Note: the source is paused; pushes are refused until it is resumed in the web app.")
	}
	return nil
}

// checkClean refuses when dest has content that matches neither the last
// pulled/pushed hash nor the remote content.
func checkClean(proj *config.Project, name, rel, sourceID string, remote []byte, state *config.State) error {
	local, err := proj.ReadRegular(name, config.MAX_IMPORT_BYTES)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if errors.Is(err, config.ErrTooLarge) {
		return fmt.Errorf("%w: %q is larger than any diagram and differs from it; pass --force to overwrite", ErrLocalChanges, rel)
	}
	if err != nil {
		return fmt.Errorf("read %q: %w", rel, err)
	}
	if bytes.Equal(local, remote) {
		return nil
	}
	if state != nil && state.SourceID == sourceID && state.File == rel &&
		state.ContentSHA256 == config.HashContent(local) {
		return nil
	}
	since := "it was never pulled here"
	if state != nil && state.File == rel {
		since = fmt.Sprintf("it changed since version %d was pulled or pushed", state.Version)
	}
	return fmt.Errorf("%w: %q differs from the diagram and %s; push it first, or pass --force to overwrite", ErrLocalChanges, rel, since)
}

// layoutDocument pretty-prints the layout (null → {}).
func layoutDocument(raw json.RawMessage) []byte {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return []byte("{}\n")
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return append(append([]byte{}, raw...), '\n')
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}
