package commands

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/aidenappl/dbdiagram-cli/config"
	"github.com/aidenappl/dbdiagram-cli/introspect"
)

// INSPECT_DEADLINE bounds a whole inspection.
const INSPECT_DEADLINE = 5 * time.Minute

const inspectUsage = `inspect --driver mysql|mariadb|postgres|sqlite --host H [--port P] --user U --database D
         [--password-env VAR] [--schema S]... [--tls require|verify-full|disable] [--tls-ca FILE]
         [--timeout 30s] [--out FILE]

The password is read from the environment variable named by --password-env,
or prompted for on a terminal. There is deliberately no --password flag and
no DSN flag. Sessions are read-only with a statement timeout; TLS defaults
to require.`

// stringList is a repeatable, comma-separated flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

// inspectFlags are shared by inspect and push --inspect.
type inspectFlags struct {
	driver, host, user, database string
	port                         int
	passwordEnv                  string
	schemas                      stringList
	tls, tlsCA                   string
	timeout                      time.Duration
}

func (f *inspectFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.driver, "driver", "", "database driver: mysql, mariadb, postgres or sqlite")
	fs.StringVar(&f.host, "host", "", "database host")
	fs.IntVar(&f.port, "port", 0, "database port (default 3306 / 5432)")
	fs.StringVar(&f.user, "user", "", "database user (use a read-only account)")
	fs.StringVar(&f.database, "database", "", "database name (SQLite: the file path)")
	fs.StringVar(&f.passwordEnv, "password-env", "", "name of the environment variable holding the password")
	fs.Var(&f.schemas, "schema", "Postgres schema to inspect (repeatable or comma-separated; default all)")
	fs.StringVar(&f.tls, "tls", introspect.TLS_REQUIRE, "TLS mode: require, verify-full or disable")
	fs.StringVar(&f.tlsCA, "tls-ca", "", "PEM CA bundle for verify-full")
	fs.DurationVar(&f.timeout, "timeout", introspect.DEFAULT_STATEMENT_TIMEOUT, "per-statement timeout")
}

// options turns the flags into introspect options, resolving the password.
func (f *inspectFlags) options(env *Env) (introspect.Options, error) {
	o := introspect.Options{
		Driver: f.driver, Host: f.host, Port: f.port, User: f.user, Database: f.database,
		Schemas: f.schemas, TLS: f.tls, TLSCA: f.tlsCA, StatementTimeout: f.timeout,
	}
	if err := o.Validate(); err != nil {
		return o, err
	}
	if o.Driver == introspect.DRIVER_SQLITE {
		return o, nil
	}
	if strings.EqualFold(f.passwordEnv, config.ENV_TOKEN) {
		// Would hand the dbdiagram token to whatever host --host names.
		return o, errors.New("--password-env must not name DBDIAGRAM_TOKEN: that is the dbdiagram API token, not a database password")
	}
	if o.TLS == introspect.TLS_REQUIRE {
		fmt.Fprintln(env.Stderr, "warning: --tls require encrypts but does not verify the server's certificate; "+
			"use --tls verify-full (with --tls-ca for a private CA) unless the network path is trusted")
	}
	switch {
	case f.passwordEnv != "":
		v, ok := env.LookupEnv(f.passwordEnv)
		if !ok {
			return o, fmt.Errorf("--password-env %s: the variable is not set", f.passwordEnv)
		}
		o.Password = v
	case env.PromptPassword != nil:
		v, err := env.PromptPassword(fmt.Sprintf("Password for %s@%s: ", o.User, o.Host))
		if err != nil {
			return o, err
		}
		o.Password = v
	}
	return o, nil
}

// inspectToDBML runs an inspection and returns DBML.
func inspectToDBML(ctx context.Context, f *inspectFlags, env *Env, verbose bool) (string, error) {
	o, err := f.options(env)
	if err != nil {
		return "", err
	}
	if verbose {
		where := o.Database
		if o.Driver != introspect.DRIVER_SQLITE {
			where = fmt.Sprintf("%s@%s:%d/%s (tls %s)", o.User, o.Host, o.Port, o.Database, o.TLS)
		}
		fmt.Fprintf(env.Stderr, "[dbdiagram] inspecting %s %s\n", o.Driver, where)
	}
	ctx, cancel := context.WithTimeout(ctx, INSPECT_DEADLINE)
	defer cancel()
	started := time.Now()
	res, err := introspect.DBML(ctx, o)
	if err != nil {
		return "", err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(env.Stderr, "warning:", config.SafeText(w))
	}
	if verbose {
		fmt.Fprintf(env.Stderr, "[dbdiagram] inspected in %s (%d bytes of DBML)\n", time.Since(started).Round(time.Millisecond), len(res.DBML))
	}
	return res.DBML, nil
}

func runInspect(ctx context.Context, args []string, env *Env) error {
	fs := newFlagSet("inspect", inspectUsage, env)
	var f inspectFlags
	f.register(fs)
	out := fs.String("out", "", "write DBML to this file instead of stdout (\"-\" = stdout)")
	verbose := fs.Bool("verbose", false, "log connection and timing (never the password)")
	if err := parse(fs, args); err != nil {
		return err
	}
	text, err := inspectToDBML(ctx, &f, env, *verbose)
	if err != nil {
		return err
	}
	if *out == "" || *out == "-" {
		_, err := fmt.Fprint(env.Stdout, text)
		return err
	}
	if err := config.WriteOut(env.Cwd, *out, []byte(text)); err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "Wrote %q\n", *out)
	return nil
}

// terminalPrompt reads a password without echo when stdin is a terminal;
// nil otherwise (no prompt is possible).
func terminalPrompt() func(string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil
	}
	return func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", errors.New("could not read the password from the terminal")
		}
		return string(b), nil
	}
}
