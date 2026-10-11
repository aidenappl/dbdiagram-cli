package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aidenappl/dbdiagram-cli/config"
)

const authUsage = "auth login [--token dbd_…] [--api URL] | auth logout | auth status"

func runAuth(_ context.Context, args []string, env *Env) error {
	if len(args) == 0 {
		fmt.Fprintf(env.Stderr, "Usage: dbdiagram %s\n", authUsage)
		return errUsage
	}
	switch args[0] {
	case "login":
		return runAuthLogin(args[1:], env)
	case "logout":
		return runAuthLogout(args[1:], env)
	case "status":
		return runAuthStatus(args[1:], env)
	}
	fmt.Fprintf(env.Stderr, "unknown auth command %q\nUsage: dbdiagram %s\n", args[0], authUsage)
	return errUsage
}

func runAuthLogin(args []string, env *Env) error {
	fs := newFlagSet("auth login", "auth login [--token dbd_…] [--api URL]\n\nWithout --token the token is read from stdin (safer: it stays out of shell history).", env)
	token := fs.String("token", "", "team API token with the sync scope (dbd_…); omit to read it from stdin")
	api := fs.String("api", "", "API origin to store with the token (default "+config.DEFAULT_API+")")
	if err := parse(fs, args); err != nil {
		return err
	}
	t := strings.TrimSpace(*token)
	if t == "" {
		if env.PromptPassword != nil {
			v, err := env.PromptPassword("API token: ")
			if err != nil {
				return err
			}
			t = strings.TrimSpace(v)
		} else {
			line, err := bufio.NewReader(io.LimitReader(env.Stdin, 4096)).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("read token from stdin: %w", err)
			}
			t = strings.TrimSpace(line)
		}
	}
	if !config.ValidToken(t) {
		return errors.New("that is not a dbdiagram API token (expected dbd_ followed by 43 characters)")
	}
	if *api != "" {
		if err := ValidateAPI(*api); err != nil {
			return err
		}
	}
	dir, err := config.Dir(env.Getenv)
	if err != nil {
		return err
	}
	if err := config.SaveCredentials(dir, config.Credentials{Token: t, API: strings.TrimRight(*api, "/")}); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Saved token %s to %q\n", config.MaskToken(t), dir)
	return nil
}

func runAuthLogout(args []string, env *Env) error {
	fs := newFlagSet("auth logout", "auth logout", env)
	if err := parse(fs, args); err != nil {
		return err
	}
	dir, err := config.Dir(env.Getenv)
	if err != nil {
		return err
	}
	removed, err := config.DeleteCredentials(dir)
	if err != nil {
		return err
	}
	if removed {
		fmt.Fprintln(env.Stdout, "Logged out (stored token removed). Revoke it in the web app if it may have leaked.")
	} else {
		fmt.Fprintln(env.Stdout, "Not logged in.")
	}
	if env.Getenv(config.ENV_TOKEN) != "" {
		fmt.Fprintln(env.Stdout, "Note: DBDIAGRAM_TOKEN is still set in this environment.")
	}
	return nil
}

func runAuthStatus(args []string, env *Env) error {
	fs := newFlagSet("auth status", "auth status", env)
	if err := parse(fs, args); err != nil {
		return err
	}
	dir, err := config.Dir(env.Getenv)
	if err != nil {
		return err
	}
	creds, err := config.LoadCredentials(dir)
	if err != nil {
		return err
	}
	token, from := env.Getenv(config.ENV_TOKEN), "DBDIAGRAM_TOKEN"
	if token == "" && creds != nil {
		token, from = creds.Token, dir
	}
	api, apiFrom := env.Getenv(config.ENV_API), "DBDIAGRAM_API"
	if api == "" && from == dir && creds.API != "" {
		api, apiFrom = creds.API, "stored login"
	}
	if api == "" {
		api, apiFrom = config.DEFAULT_API, "default"
	}
	if token == "" {
		fmt.Fprintln(env.Stdout, "Not logged in.")
		fmt.Fprintf(env.Stdout, "API: %s (%s)\n", api, apiFrom)
		return errors.New("no token: run `dbdiagram auth login` or set DBDIAGRAM_TOKEN")
	}
	fmt.Fprintf(env.Stdout, "Token: %s (from %s)\n", config.MaskToken(token), config.SafeText(from))
	fmt.Fprintf(env.Stdout, "API:   %s (%s)\n", api, apiFrom)
	if !config.ValidToken(token) {
		fmt.Fprintln(env.Stdout, "Warning: the token does not have the dbd_ API token shape.")
	}
	if from == dir && config.CredentialsInsecure(dir) {
		fmt.Fprintln(env.Stdout, "Warning: the credentials file is readable by other users; run `chmod 600` on it.")
	}
	if from == dir && config.ConfigDirWritable(dir) {
		fmt.Fprintln(env.Stdout, "Warning: the config directory is writable by other users; run `chmod 700` on it (the next `auth login` does).")
	}
	return nil
}
