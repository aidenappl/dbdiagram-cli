// Package commands implements the dbdiagram CLI's subcommands with the
// standard library's flag package.
package commands

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aidenappl/dbdiagram-cli/client"
	"github.com/aidenappl/dbdiagram-cli/config"
)

// Env is everything a command touches outside its arguments, so tests can
// run commands in-process.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// LookupEnv reads an environment variable.
	LookupEnv func(string) (string, bool)
	// Cwd is the directory commands resolve the project from.
	Cwd     string
	Version string
	// HTTPClient overrides the API client's transport (tests).
	HTTPClient *http.Client
	// Sleep overrides the API client's retry wait (tests).
	Sleep func(ctx context.Context, d time.Duration) error
	// PromptPassword reads a password without echo; nil means "not a
	// terminal" (no prompt).
	PromptPassword func(prompt string) (string, error)
}

// Getenv returns an environment variable or "".
func (e *Env) Getenv(k string) string {
	v, _ := e.LookupEnv(k)
	return v
}

// errUsage marks a usage error: exit code 2, usage already printed.
var errUsage = errors.New("usage")

const rootUsage = `dbdiagram — sync database schemas with dbdiagram (appleby.cloud)

Usage:
  dbdiagram <command> [flags]

Commands:
  auth login|logout|status   store, remove or show the API token
  init                       write .dbdiagram/settings.json for this project
  push                       push the entry file (or --stdin / --inspect) to the source
  pull                       write the diagram's DBML and layout to disk
  inspect                    read a live database's schema and print DBML
  version                    print the version

Run 'dbdiagram <command> -h' for a command's flags.
Environment: DBDIAGRAM_TOKEN, DBDIAGRAM_API override the stored login.
`

// Run executes args and returns the process exit code.
func Run(ctx context.Context, args []string, env *Env) int {
	err := dispatch(ctx, args, env)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	}
	// Errors can echo paths and server text: never let them carry terminal
	// escape sequences.
	fmt.Fprintln(env.Stderr, "error:", config.SafeText(err.Error()))
	return 1
}

func dispatch(ctx context.Context, args []string, env *Env) error {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, rootUsage)
		return errUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "auth":
		return runAuth(ctx, rest, env)
	case "init":
		return runInit(rest, env)
	case "push":
		return runPush(ctx, rest, env)
	case "pull":
		return runPull(ctx, rest, env)
	case "inspect":
		return runInspect(ctx, rest, env)
	case "version", "--version", "-v":
		return runVersion(env)
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, rootUsage)
		return nil
	}
	fmt.Fprintf(env.Stderr, "unknown command %q\n\n%s", cmd, rootUsage)
	return errUsage
}

// newFlagSet returns a ContinueOnError flag set writing to stderr.
func newFlagSet(name, usage string, env *Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: dbdiagram %s\n\nFlags:\n", usage)
		fs.PrintDefaults()
	}
	return fs
}

// parse parses flags, mapping parse errors to errUsage and refusing
// unexpected positional arguments.
func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return errUsage
	}
	return nil
}

// apiFlags are shared by push and pull.
type apiFlags struct {
	api     string
	verbose bool
}

func (a *apiFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&a.api, "api", "", "API origin (default $DBDIAGRAM_API, the stored login's, or "+config.DEFAULT_API+")")
	fs.BoolVar(&a.verbose, "verbose", false, "log each API request (method, path, status, timing; never the token)")
}

// newClient resolves the token and API origin: flag > environment > stored
// credentials > default.
func newClient(env *Env, a apiFlags) (*client.Client, error) {
	dir, err := config.Dir(env.Getenv)
	if err != nil {
		return nil, err
	}
	creds, err := config.LoadCredentials(dir)
	if err != nil {
		return nil, err
	}
	// The stored API origin is used only with the stored token: an env token
	// never gets sent to an origin some earlier login wrote to disk.
	token, storedAPI := env.Getenv(config.ENV_TOKEN), ""
	if token == "" && creds != nil {
		token, storedAPI = creds.Token, creds.API
	}
	if token == "" {
		return nil, errors.New("not logged in: run `dbdiagram auth login --token dbd_…` or set DBDIAGRAM_TOKEN")
	}
	if !config.ValidToken(token) {
		return nil, errors.New("the configured token is not a dbdiagram API token (dbd_ + 43 characters)")
	}
	api := firstNonEmpty(a.api, env.Getenv(config.ENV_API), storedAPI, config.DEFAULT_API)
	if err := ValidateAPI(api); err != nil {
		return nil, err
	}
	c := client.New(api, token, "dbdiagram-cli/"+resolveVersion(env.Version))
	if env.HTTPClient != nil {
		c.HTTP = client.NoRedirects(env.HTTPClient)
	}
	if env.Sleep != nil {
		c.Sleep = env.Sleep
	}
	if a.verbose {
		c.Verbose = env.Stderr
	}
	return c, nil
}

// ValidateAPI requires an absolute https origin (http only for loopback, so
// a token never crosses a network in clear text).
func ValidateAPI(api string) error {
	u, err := url.Parse(api)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("API %q must be an origin like https://dbdiagram-api.appleby.cloud", api)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := strings.ToLower(u.Hostname())
		if h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
		return fmt.Errorf("API %q must use https (http is allowed only for localhost)", api)
	}
	return fmt.Errorf("API %q must use https", api)
}

// projectRoot finds the project root from env.Cwd.
func projectRoot(env *Env) (string, *config.Settings, error) {
	root, err := config.FindRoot(env.Cwd)
	if err != nil {
		return "", nil, err
	}
	s, err := config.LoadSettings(root)
	if err != nil {
		return "", nil, err
	}
	return root, s, nil
}

// DefaultEnv is the real process environment.
func DefaultEnv(version string) *Env {
	cwd, _ := os.Getwd()
	return &Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		LookupEnv: os.LookupEnv, Cwd: cwd, Version: version,
		PromptPassword: terminalPrompt(),
	}
}
