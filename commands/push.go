package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/aidenappl/dbdiagram-cli/client"
	"github.com/aidenappl/dbdiagram-cli/config"
)

const pushUsage = `push [--message M] [--sha SHA|auto] [--ref REF|auto] [--source ID] [--entry FILE] [--format F]
       [--stdin | --inspect <inspect flags>] [--api URL] [--verbose]

Pushes the entry file from .dbdiagram/settings.json (or --stdin, or a live
--inspect) to the source. --sha/--ref default to the git HEAD of the project.`

// shaPattern mirrors the API's sha validation.
var shaPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

const MAX_REF_CHARS = 255

func runPush(ctx context.Context, args []string, env *Env) error {
	fs := newFlagSet("push", pushUsage, env)
	var a apiFlags
	a.register(fs)
	message := fs.String("message", "", "version label (first line, ≤ 100 characters)")
	sha := fs.String("sha", "auto", "commit sha to record (auto = git HEAD / $GITHUB_SHA; empty = none)")
	ref := fs.String("ref", "auto", "branch or ref to record (auto = current branch / $GITHUB_REF_NAME; empty = none)")
	source := fs.String("source", "", "source id (overrides settings)")
	entry := fs.String("entry", "", "file to push (overrides settings)")
	format := fs.String("format", "", "content format: dbml, mysql, postgres, mssql or schemarb")
	stdin := fs.Bool("stdin", false, "read the content from stdin (default format dbml)")
	useInspect := fs.Bool("inspect", false, "inspect a live database and push the DBML (takes the inspect flags)")
	var insp inspectFlags
	insp.register(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	if *stdin && *useInspect {
		return errors.New("--stdin and --inspect are mutually exclusive")
	}

	root, settings, err := projectRoot(env)
	if err != nil && !errors.Is(err, config.ErrNoSettings) {
		return err
	}
	if settings == nil {
		settings = &config.Settings{}
		root = env.Cwd
	}
	sourceID := firstNonEmpty(*source, settings.SourceID)
	if sourceID == "" {
		return errors.New("no source: pass --source or run `dbdiagram init`")
	}
	if !config.ValidSourceID(sourceID) {
		return fmt.Errorf("source id %q is not valid", sourceID)
	}

	var (
		content  []byte
		fmtName  string
		fromFile string // root-relative entry path when the content is a project file
	)
	switch {
	case *useInspect:
		if *format != "" && *format != "dbml" {
			return errors.New("--inspect always pushes DBML")
		}
		text, err := inspectToDBML(ctx, &insp, env, a.verbose)
		if err != nil {
			return err
		}
		content, fmtName = []byte(text), "dbml"
	case *stdin:
		fmtName = firstNonEmpty(*format, "dbml")
		if !config.ValidFormat(fmtName) {
			return fmt.Errorf("--format %q must be one of %s", fmtName, strings.Join(config.FORMATS, ", "))
		}
		b, err := io.ReadAll(io.LimitReader(env.Stdin, int64(config.MaxBytes(fmtName))+1))
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		content = b
	default:
		e := firstNonEmpty(*entry, settings.Entry)
		if e == "" {
			return errors.New("no entry file: pass --entry, --stdin or --inspect, or run `dbdiagram init`")
		}
		fmtName = firstNonEmpty(*format, settings.Format, "dbml")
		if *entry != "" && *format == "" && *entry != settings.Entry {
			fmtName = "dbml"
		}
		if !config.ValidFormat(fmtName) {
			return fmt.Errorf("--format %q must be one of %s", fmtName, strings.Join(config.FORMATS, ", "))
		}
		if *entry != "" {
			rel, ok := config.RelInside(root, absFrom(env.Cwd, e))
			if !ok {
				return fmt.Errorf("--entry %q is outside the project %q; use --stdin to push other files", e, root)
			}
			e = rel
		}
		if err := config.ValidateEntry(e); err != nil {
			return err
		}
		if content, err = readEntry(root, e, fmtName); err != nil {
			return err
		}
		fromFile = e
	}
	if err := config.CheckSize(fmtName, len(content)); err != nil {
		return err
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return errors.New("nothing to push: the content is empty")
	}

	req := client.PushRequest{DBML: string(content), Format: fmtName, Message: strings.TrimSpace(*message)}
	req.SHA, req.Ref, err = resolveGit(env, root, *sha, *ref)
	if err != nil {
		return err
	}
	c, err := newClient(env, a)
	if err != nil {
		return err
	}
	res, err := c.Push(ctx, sourceID, req)
	if err != nil {
		return err
	}
	printPush(env, res)

	// The working copy now matches the server: remember it so pull knows
	// the file has no unpushed changes.
	if fromFile != "" && fmtName == "dbml" && settings.SourceID == sourceID {
		_ = config.SaveState(root, config.State{
			SourceID: sourceID, Version: res.Diagram.Version, File: fromFile,
			ContentSHA256: config.HashContent(content), UpdatedAt: time.Now().UTC(),
		})
	}
	return nil
}

func printPush(env *Env, res *client.PushResult) {
	name := res.Diagram.Name
	if res.Run.Status == "skipped" {
		fmt.Fprintf(env.Stdout, "No changes: %q is already at version %d.\n", name, res.Diagram.Version)
		return
	}
	fmt.Fprintf(env.Stdout, "Pushed to %q: version %d (run %s, %s, %d bytes).\n",
		name, res.Diagram.Version, config.SafeText(res.Run.Status), config.SafeText(res.Run.Format), res.Run.InputBytes)
	if res.Compile != nil && !res.Compile.ParseOK {
		fmt.Fprintf(env.Stderr, "warning: the DBML has %d error(s); it was stored, but the diagram shows the last valid model:\n", res.Compile.DiagnosticsCount)
		for i, d := range res.Compile.Diagnostics {
			if i == 5 {
				fmt.Fprintf(env.Stderr, "  … and %d more\n", res.Compile.DiagnosticsCount-5)
				break
			}
			fmt.Fprintf(env.Stderr, "  - %s\n", config.SafeText(d.Message))
		}
	}
}

// resolveGit fills sha/ref: an explicit value wins, "auto" asks git (then
// the GitHub Actions environment), "" sends nothing.
func resolveGit(env *Env, root, sha, ref string) (string, string, error) {
	if sha == "auto" {
		sha = gitOutput(root, "rev-parse", "HEAD")
		if sha == "" {
			sha = env.Getenv("GITHUB_SHA")
		}
		if !shaPattern.MatchString(sha) {
			sha = ""
		}
	} else if sha != "" && !shaPattern.MatchString(sha) {
		return "", "", errors.New("--sha must be 1-64 of A-Z a-z 0-9 . _ -")
	}
	if ref == "auto" {
		ref = gitOutput(root, "rev-parse", "--abbrev-ref", "HEAD")
		if ref == "" || ref == "HEAD" {
			ref = firstNonEmpty(env.Getenv("GITHUB_HEAD_REF"), env.Getenv("GITHUB_REF_NAME"))
		}
		if !validRef(ref) {
			ref = ""
		}
	} else if ref != "" && !validRef(ref) {
		return "", "", fmt.Errorf("--ref must be ≤ %d printable characters", MAX_REF_CHARS)
	}
	return sha, ref, nil
}

func validRef(ref string) bool {
	if ref == "" || len([]rune(ref)) > MAX_REF_CHARS {
		return false
	}
	for _, r := range ref {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// readEntry reads a project entry through the project root: a regular file
// (never a symlink, device or FIFO), bounded by the format's size limit.
func readEntry(root, entry, format string) ([]byte, error) {
	proj, err := config.OpenProject(root)
	if err != nil {
		return nil, err
	}
	defer proj.Close()
	b, err := proj.ReadRegular(entry, int64(config.MaxBytes(format)))
	if errors.Is(err, config.ErrTooLarge) {
		if fi, lerr := proj.Lstat(entry); lerr == nil {
			if serr := config.CheckSize(format, int(fi.Size())); serr != nil {
				return nil, serr
			}
		}
		return nil, config.CheckSize(format, config.MaxBytes(format)+1)
	}
	if err != nil {
		return nil, fmt.Errorf("entry file: %w", err)
	}
	return b, nil
}

// absFrom resolves p against base unless it is absolute.
func absFrom(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, filepath.FromSlash(p))
}
