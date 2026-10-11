package commands

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aidenappl/dbdiagram-cli/config"
)

// Regression tests for the Task 4.2 security review (review-42): every
// repro from the review is one case here.

func writeSettings(t *testing.T, dir, entry string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, config.DIR_NAME), 0o755); err != nil {
		t.Fatal(err)
	}
	s := `{"source_id":"src-1","entry":"` + entry + `","format":"dbml"}`
	if err := os.WriteFile(filepath.Join(dir, config.DIR_NAME, config.SETTINGS_FILE), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// HIGH1: a committed `home -> ~` symlink plus entry home/.zshenv made pull
// create the victim's ~/.zshenv.
func TestPullRefusesSymlinkedDirectoryEscape(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	victim := t.TempDir()
	if err := os.Symlink(victim, filepath.Join(h.env.Cwd, "home")); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, h.env.Cwd, "home/.zshenv")
	h.api.dbml, h.api.version = "echo pwned\n", 1
	if code := h.run("pull", "--force"); code != 1 {
		t.Fatalf("pull through a symlinked dir: code %d, stderr %s", code, h.stderr)
	}
	for _, f := range []string{".zshenv", ".zshenv.dbdiagram", ".zshenv.dbdiagram"} {
		if _, err := os.Lstat(filepath.Join(victim, f)); err == nil {
			t.Fatalf("pull wrote %s outside the project", f)
		}
	}
	if entries, _ := os.ReadDir(victim); len(entries) != 0 {
		t.Fatalf("victim dir touched: %v", entries)
	}
}

// HIGH1: the destination itself being a symlink (even inside the project)
// is refused, with or without --force; the target is untouched.
func TestPullRefusesSymlinkDestination(t *testing.T) {
	for _, inside := range []bool{true, false} {
		t.Run(map[bool]string{true: "inside", false: "outside"}[inside], func(t *testing.T) {
			h := newHarness(t)
			h.login(t)
			h.run("init", "--source", "src-1")
			target := filepath.Join(t.TempDir(), "target.txt")
			if inside {
				target = filepath.Join(h.env.Cwd, "other.txt")
			}
			if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(h.env.Cwd, "schema.dbml")); err != nil {
				t.Fatal(err)
			}
			h.api.dbml, h.api.version = "Table a {}\n", 1
			if code := h.run("pull", "--force"); code != 1 || !strings.Contains(h.stderr.String(), "symbolic link") {
				t.Fatalf("pull: %d %s", code, h.stderr)
			}
			if b, _ := os.ReadFile(target); string(b) != "original" {
				t.Fatal("symlink target was overwritten")
			}
		})
	}
}

// HIGH1: a committed .dbdiagram symlink cannot redirect settings/state.
func TestDotDbdiagramSymlinkEscapeRefused(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	outside := t.TempDir()
	writeSettings(t, outside, "schema.dbml")
	if err := os.Symlink(filepath.Join(outside, config.DIR_NAME), filepath.Join(h.env.Cwd, config.DIR_NAME)); err != nil {
		t.Fatal(err)
	}
	h.write(t, "schema.dbml", "Table a {}\n")
	if code := h.run("push"); code != 1 {
		t.Fatalf("push via a symlinked .dbdiagram: %d", code)
	}
	if h.api.requests != 0 {
		t.Fatal("request sent")
	}
}

// --out is the explicit escape hatch: outside the project is fine when
// typed on the command line, but never through a final symlink, and never a
// layout file name.
func TestPullOutEscapeHatch(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	h.run("init", "--source", "src-1")
	h.api.dbml, h.api.version = "Table a {}\n", 1
	elsewhere := filepath.Join(t.TempDir(), "sub", "x.dbml")
	if code := h.run("pull", "--out", elsewhere); code != 0 {
		t.Fatalf("pull --out outside: %d %s", code, h.stderr)
	}
	if b, _ := os.ReadFile(elsewhere); string(b) != "Table a {}\n" {
		t.Fatal("--out not written")
	}
	link := filepath.Join(t.TempDir(), "link.dbml")
	victim := filepath.Join(t.TempDir(), "v")
	_ = os.WriteFile(victim, []byte("keep"), 0o644)
	_ = os.Symlink(victim, link)
	if code := h.run("pull", "--force", "--out", link); code != 1 {
		t.Fatalf("pull --out symlink: %d", code)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatal("--out followed a symlink")
	}
	if code := h.run("pull", "--out", "x.dbdiagram"); code != 1 || !strings.Contains(h.stderr.String(), "layout") {
		t.Fatalf("--out layout name: %d %s", code, h.stderr)
	}
}

// HIGH2: a symlinked entry uploaded ~/.config/secret.txt.
func TestPushRefusesSymlinkedEntry(t *testing.T) {
	for _, inside := range []bool{false, true} {
		t.Run(map[bool]string{true: "inside", false: "outside"}[inside], func(t *testing.T) {
			h := newHarness(t)
			h.login(t)
			h.run("init", "--source", "src-1")
			secret := filepath.Join(t.TempDir(), "secret.txt")
			if inside {
				secret = filepath.Join(h.env.Cwd, "secret.txt")
			}
			_ = os.WriteFile(secret, []byte("TOP SECRET"), 0o644)
			if err := os.Symlink(secret, filepath.Join(h.env.Cwd, "schema.dbml")); err != nil {
				t.Fatal(err)
			}
			if code := h.run("push"); code != 1 || !strings.Contains(h.stderr.String(), "symbolic link") {
				t.Fatalf("push: %d %s", code, h.stderr)
			}
			if h.api.requests != 0 {
				t.Fatal("a symlinked entry reached the API")
			}
		})
	}
}

// HIGH2: entries in repository or CLI metadata are refused, from settings
// and from --entry alike; --entry outside the project is refused.
func TestPushRefusesMetadataEntries(t *testing.T) {
	for _, entry := range []string{".git/config", "sub/.git/config", ".github/workflows/x.yml", ".gitmodules", ".dbdiagram/state.json", "schema.dbdiagram", "a/.GIT/x"} {
		t.Run(entry, func(t *testing.T) {
			h := newHarness(t)
			h.login(t)
			writeSettings(t, h.env.Cwd, entry)
			h.write(t, entry, "Table a {}\n")
			if code := h.run("push"); code != 1 {
				t.Fatalf("push from settings: %d", code)
			}
			h.run("init", "--source", "src-1", "--force")
			if code := h.run("push", "--entry", entry); code != 1 {
				t.Fatalf("push --entry: %d", code)
			}
			if h.api.requests != 0 {
				t.Fatal("metadata reached the API")
			}
		})
	}
	h := newHarness(t)
	h.login(t)
	h.run("init", "--source", "src-1")
	outside := filepath.Join(t.TempDir(), "x.dbml")
	_ = os.WriteFile(outside, []byte("Table a {}\n"), 0o644)
	if code := h.run("push", "--entry", outside); code != 1 || !strings.Contains(h.stderr.String(), "outside the project") {
		t.Fatalf("--entry outside: %d %s", code, h.stderr)
	}
}

// MED4: a redirect was followed (the review's redirector → sink). The token
// is never replayed and the 3xx is an error.
func TestRedirectIsNotFollowed(t *testing.T) {
	var sinkHits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkHits.Add(1)
		w.WriteHeader(500)
	}))
	defer sink.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	h := newHarness(t)
	h.login(t)
	h.vars[config.ENV_API] = redir.URL
	h.run("init", "--source", "src-1")
	h.write(t, "schema.dbml", "Table a {}\n")
	for _, args := range [][]string{{"push"}, {"pull"}} {
		if code := h.run(args...); code != 1 || !strings.Contains(h.stderr.String(), "redirect") {
			t.Fatalf("%v: %d %s", args, code, h.stderr)
		}
	}
	if sinkHits.Load() != 0 {
		t.Fatalf("redirect followed %d times", sinkHits.Load())
	}
}

// LOW7: a stored API origin is only paired with the stored token.
func TestEnvTokenNeverUsesStoredAPI(t *testing.T) {
	h := newHarness(t)
	delete(h.vars, config.ENV_API)
	if code := h.run("auth", "login", "--token", testToken, "--api", "https://stored.example.com"); code != 0 {
		t.Fatal(h.stderr)
	}
	c, err := newClient(h.env, apiFlags{})
	if err != nil || c.BaseURL != "https://stored.example.com" {
		t.Fatalf("stored token: %v %v", c, err)
	}
	h.vars[config.ENV_TOKEN] = "dbd_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	c, err = newClient(h.env, apiFlags{})
	if err != nil || c.BaseURL != config.DEFAULT_API {
		t.Fatalf("env token must use the default API, got %v %v", c.BaseURL, err)
	}
	h.vars[config.ENV_API] = "https://env.example.com"
	if c, _ = newClient(h.env, apiFlags{}); c.BaseURL != "https://env.example.com" {
		t.Fatalf("env API: %s", c.BaseURL)
	}
	h.run("auth", "status")
	if strings.Contains(h.stdout.String(), "stored.example.com") {
		t.Fatalf("status pairs the env token with the stored API: %s", h.stdout)
	}
}

// LOW10/LOW11: --password-env DBDIAGRAM_TOKEN is refused; --tls require warns.
func TestInspectPasswordEnvAndTLSWarning(t *testing.T) {
	h := newHarness(t)
	h.vars[config.ENV_TOKEN] = testToken
	if code := h.run("inspect", "--driver", "mysql", "--host", "127.0.0.1", "--port", "1", "--user", "u", "--database", "d", "--password-env", "DBDIAGRAM_TOKEN"); code != 1 || !strings.Contains(h.stderr.String(), "must not name DBDIAGRAM_TOKEN") {
		t.Fatalf("password-env token: %d %s", code, h.stderr)
	}
	h.vars["DBPW"] = "x"
	h.run("inspect", "--driver", "mysql", "--host", "127.0.0.1", "--port", "1", "--user", "u", "--database", "d", "--password-env", "DBPW", "--timeout", "1s")
	if !strings.Contains(h.stderr.String(), "verify-full") {
		t.Fatalf("no tls warning: %s", h.stderr)
	}
	h.run("inspect", "--driver", "mysql", "--host", "127.0.0.1", "--port", "1", "--user", "u", "--database", "d", "--password-env", "DBPW", "--tls", "disable", "--timeout", "500ms")
	if !strings.Contains(h.stderr.String(), "at least 1s") {
		t.Fatalf("timeout floor: %s", h.stderr)
	}
}

// Round 3 (reverify-42): metadata reached through in-project symlinks.

func gitRepoWithAlias(t *testing.T, h *harness, alias, target string) {
	t.Helper()
	for _, d := range []string{".git/hooks", ".dbdiagram"} {
		if err := os.MkdirAll(filepath.Join(h.env.Cwd, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.write(t, ".git/config", "[http \"https://github.com/\"]\n\textraheader = AUTHORIZATION: basic SECRET\n")
	if err := os.Symlink(target, filepath.Join(h.env.Cwd, alias)); err != nil {
		t.Fatal(err)
	}
}

func TestPushRefusesGitConfigThroughSymlinkedDir(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	gitRepoWithAlias(t, h, "g", ".git")
	writeSettings(t, h.env.Cwd, "g/config")
	if code := h.run("push"); code != 1 || !strings.Contains(h.stderr.String(), "symbolic link") {
		t.Fatalf("push g/config: %d %s", code, h.stderr)
	}
	h.run("init", "--source", "src-1", "--force")
	if code := h.run("push", "--entry", "g/config"); code != 1 {
		t.Fatalf("push --entry g/config: %d", code)
	}
	for _, p := range h.api.pushes {
		if strings.Contains(p["dbml"].(string), "SECRET") {
			t.Fatal(".git/config uploaded")
		}
	}
	if h.api.requests != 0 {
		t.Fatal("request sent")
	}
}

func TestPullRefusesGitHooksThroughSymlinkedDir(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	gitRepoWithAlias(t, h, "g", ".git")
	writeSettings(t, h.env.Cwd, "g/hooks/post-checkout")
	h.api.dbml, h.api.version = "#!/bin/sh\necho pwned\n", 1
	if code := h.run("pull", "--force"); code != 1 {
		t.Fatalf("pull into hooks: %d", code)
	}
	if code := h.run("pull", "--force", "--out", "g/hooks/post-checkout"); code != 1 {
		t.Fatalf("pull --out into hooks: %d", code)
	}
	if code := h.run("pull", "--force", "--out", ".git/hooks/post-checkout"); code != 1 {
		t.Fatalf("pull --out .git/hooks: %d", code)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.env.Cwd, ".git", "hooks")); len(entries) != 0 {
		t.Fatalf("wrote into .git/hooks: %v", entries)
	}
}

func TestPullRefusesStateThroughSymlinkedDir(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	h.run("init", "--source", "src-1")
	gitRepoWithAlias(t, h, "m", ".dbdiagram")
	state := filepath.Join(h.env.Cwd, ".dbdiagram", "state.json")
	_ = os.WriteFile(state, []byte(`{"version":1}`), 0o644)
	h.api.dbml, h.api.version = "{\"evil\":true}\n", 9
	for _, args := range [][]string{{"pull", "--force", "--out", "m/state.json"}, {"pull", "--force", "--out", "m/x.dbml"}} {
		if code := h.run(args...); code != 1 {
			t.Fatalf("%v: %d", args, code)
		}
	}
	writeSettings(t, h.env.Cwd, "m/state.json")
	if code := h.run("pull", "--force"); code != 1 {
		t.Fatalf("entry m/state.json: %d", code)
	}
	if b, _ := os.ReadFile(state); string(b) != `{"version":1}` {
		t.Fatalf("state.json overwritten: %s", b)
	}
}

func TestControlCharactersNeverReachTheTerminal(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	writeSettings(t, h.env.Cwd, `\u001b]0;PWNED\u0007\u001b[31mred.dbml`)
	if code := h.run("push"); code != 1 {
		t.Fatalf("push: %d", code)
	}
	if strings.ContainsAny(h.stderr.String()+h.stdout.String(), "\x1b\x07") {
		t.Fatalf("escape sequence printed: %q", h.stderr.String())
	}
	if code := h.run("pull", "--out", "a\x1b[31m.dbml"); code != 1 || strings.Contains(h.stderr.String(), "\x1b") {
		t.Fatalf("--out control chars: %d %q", code, h.stderr.String())
	}
}

func TestInitWritesSettingsLast(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(filepath.Join(h.env.Cwd, config.DIR_NAME), 0o755); err != nil {
		t.Fatal(err)
	}
	// A refusal (here: .gitignore is a symlink) must not leave settings.json.
	_ = os.Symlink(filepath.Join(t.TempDir(), "x"), filepath.Join(h.env.Cwd, config.DIR_NAME, config.GITIGNORE_FILE))
	if code := h.run("init", "--source", "src-1"); code != 1 {
		t.Fatalf("init: %d", code)
	}
	if _, err := os.Lstat(filepath.Join(h.env.Cwd, config.DIR_NAME, config.SETTINGS_FILE)); err == nil {
		t.Fatal("partial init left settings.json")
	}
}

func TestOutDashIsStdout(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	h.run("init", "--source", "src-1")
	h.api.dbml, h.api.version = "Table a {}\n", 1
	if code := h.run("pull", "--out", "-"); code != 0 || h.stdout.String() != "Table a {}\n" {
		t.Fatalf("pull --out -: %d %q %s", code, h.stdout, h.stderr)
	}
	if _, err := os.Lstat(filepath.Join(h.env.Cwd, "-")); err == nil {
		t.Fatal("a file named - was written")
	}
	dbPath := filepath.Join(t.TempDir(), "app.db")
	if err := runSQL(t, dbPath, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if code := h.run("inspect", "--driver", "sqlite", "--database", dbPath, "--out", "-"); code != 0 || !strings.Contains(h.stdout.String(), "Table t") {
		t.Fatalf("inspect --out -: %d %q", code, h.stdout)
	}
}
