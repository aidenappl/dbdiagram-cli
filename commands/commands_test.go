package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aidenappl/dbdiagram-cli/config"
)

const testToken = "dbd_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"

// fakeAPI mimics dbdiagram-api's push/pull contract for one source.
type fakeAPI struct {
	mu       sync.Mutex
	dbml     string
	layout   string
	version  int
	paused   bool
	pushes   []map[string]any
	requests int
	// next429 makes the next N requests answer 429 with Retry-After: 2.
	next429 int
}

func (f *fakeAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		reply := func(status int, v map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		fail := func(status, code int, msg string) {
			reply(status, map[string]any{"success": false, "error": msg, "error_message": msg, "error_code": code})
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			fail(401, 401, "authentication required")
			return
		}
		if f.next429 > 0 {
			f.next429--
			w.Header().Set("Retry-After", "2")
			fail(429, 429, "too many requests")
			return
		}
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/sources/src-1/push":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				fail(400, 400, "bad body")
				return
			}
			f.pushes = append(f.pushes, body)
			if f.paused {
				fail(409, 409, "this source is paused (the diagram is detached); resume it to sync")
				return
			}
			content, _ := body["dbml"].(string)
			if len(content) > config.MAX_DBML_BYTES {
				fail(413, 413, "diagram source is larger than 1 MiB")
				return
			}
			status, msg := "succeeded", "pushed"
			var created any
			if content == f.dbml {
				status, msg = "skipped", "no changes"
			} else {
				f.dbml = content
				f.version++
				created = f.version
			}
			reply(200, map[string]any{"success": true, "message": msg, "data": map[string]any{
				"run":     map[string]any{"id": "run", "trigger": "push", "status": status, "format": body["format"], "input_bytes": len(content), "version_created": created},
				"diagram": map[string]any{"id": "d1", "name": "Main", "version": f.version},
				"compile": map[string]any{"parse_ok": true, "diagnostics_count": 0, "diagnostics": []any{}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sources/src-1/pull":
			var layout any
			_ = json.Unmarshal([]byte(f.layout), &layout)
			st := "active"
			if f.paused {
				st = "paused"
			}
			reply(200, map[string]any{"success": true, "message": "OK", "data": map[string]any{
				"source":  map[string]any{"id": "src-1", "kind": "cli", "status": st},
				"diagram": map[string]any{"id": "d1", "name": "Main", "version": f.version, "dbml": f.dbml, "updated_at": time.Now().UTC()},
				"layout":  layout,
			}})
		default:
			fail(404, 404, "resource not found")
		}
	}
}

type harness struct {
	env    *Env
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	api    *fakeAPI
	srv    *httptest.Server
	slept  []time.Duration
	vars   map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, api: &fakeAPI{layout: `{"tables":{"public.users":{"x":10,"y":20}}}`}}
	h.srv = httptest.NewServer(h.api.handler(t))
	t.Cleanup(h.srv.Close)
	h.vars = map[string]string{
		config.ENV_CONFIG_DIR: filepath.Join(t.TempDir(), "cfg"),
		config.ENV_API:        h.srv.URL,
	}
	h.env = &Env{
		Stdin: strings.NewReader(""), Stdout: h.stdout, Stderr: h.stderr,
		LookupEnv: func(k string) (string, bool) { v, ok := h.vars[k]; return v, ok },
		Cwd:       t.TempDir(), Version: "test",
		Sleep: func(_ context.Context, d time.Duration) error { h.slept = append(h.slept, d); return nil },
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	return Run(context.Background(), args, h.env)
}

func (h *harness) login(t *testing.T) {
	t.Helper()
	if code := h.run("auth", "login", "--token", testToken); code != 0 {
		t.Fatalf("login: %d %s", code, h.stderr)
	}
}

func (h *harness) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(h.env.Cwd, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.env.Cwd, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCommandParsing(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"no args", nil, 2, "Usage"},
		{"unknown", []string{"frobnicate"}, 2, "unknown command"},
		{"help", []string{"help"}, 0, ""},
		{"push -h", []string{"push", "-h"}, 0, "Usage: dbdiagram push"},
		{"bad flag", []string{"push", "--nope"}, 2, "flag provided but not defined"},
		{"positional", []string{"pull", "extra"}, 2, "unexpected argument"},
		{"auth no sub", []string{"auth"}, 2, "Usage"},
		{"auth unknown", []string{"auth", "whoami"}, 2, "unknown auth command"},
		{"init needs source", []string{"init"}, 2, "--source is required"},
		{"init sql needs format", []string{"init", "--source", "s", "--entry", "schema.sql"}, 1, "--format"},
		{"stdin and inspect", []string{"push", "--stdin", "--inspect"}, 1, "mutually exclusive"},
		{"push without project", []string{"push"}, 1, "no source"},
		{"pull without project", []string{"pull"}, 1, "dbdiagram init"},
		{"inspect no driver", []string{"inspect"}, 1, "--driver is required"},
		{"no password flag", []string{"inspect", "--password", "x"}, 2, "flag provided but not defined"},
		{"no dsn flag", []string{"inspect", "--dsn", "x"}, 2, "flag provided but not defined"},
		{"login bad token", []string{"auth", "login", "--token", "nope"}, 1, "not a dbdiagram API token"},
		{"login http api", []string{"auth", "login", "--token", testToken, "--api", "http://example.com"}, 1, "https"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			code := h.run(c.args...)
			if code != c.wantCode {
				t.Fatalf("code = %d, want %d; stderr: %s", code, c.wantCode, h.stderr)
			}
			if c.wantErr != "" && !strings.Contains(h.stderr.String()+h.stdout.String(), c.wantErr) {
				t.Fatalf("output %q does not contain %q", h.stderr.String()+h.stdout.String(), c.wantErr)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	if h.run("version") != 0 || !strings.HasPrefix(h.stdout.String(), "dbdiagram test (") {
		t.Fatalf("version = %q", h.stdout)
	}
}

func TestAuthLoginStatusLogout(t *testing.T) {
	h := newHarness(t)
	delete(h.vars, config.ENV_API)
	if code := h.run("auth", "status"); code != 1 || !strings.Contains(h.stdout.String(), "Not logged in") {
		t.Fatalf("status before login: %d %q", code, h.stdout)
	}
	h.env.Stdin = strings.NewReader(testToken + "\n") // token from stdin, not argv
	if code := h.run("auth", "login", "--api", "https://dbdiagram-api.example.com/"); code != 0 {
		t.Fatalf("login: %d %s", code, h.stderr)
	}
	if strings.Contains(h.stdout.String(), testToken) {
		t.Fatal("login printed the token")
	}
	path := filepath.Join(h.vars[config.ENV_CONFIG_DIR], config.CREDENTIALS_FILE)
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v %v", st, err)
	}
	if code := h.run("auth", "status"); code != 0 {
		t.Fatalf("status: %d %s", code, h.stderr)
	}
	out := h.stdout.String()
	if strings.Contains(out, testToken) || !strings.Contains(out, "dbd_abcdefgh…") || !strings.Contains(out, "https://dbdiagram-api.example.com (stored login)") {
		t.Fatalf("status output = %q", out)
	}
	h.vars[config.ENV_TOKEN] = "dbd_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	h.run("auth", "status")
	if !strings.Contains(h.stdout.String(), "dbd_ZZZZZZZZ… (from DBDIAGRAM_TOKEN)") {
		t.Fatalf("env override not shown: %q", h.stdout)
	}
	delete(h.vars, config.ENV_TOKEN)
	if code := h.run("auth", "logout"); code != 0 || !strings.Contains(h.stdout.String(), "Logged out") {
		t.Fatalf("logout: %d %q", code, h.stdout)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("logout left the credentials file")
	}
}

func TestInitWritesSettings(t *testing.T) {
	h := newHarness(t)
	if code := h.run("init", "--source", "src-1", "--entry", "db/schema.dbml"); code != 0 {
		t.Fatalf("init: %d %s", code, h.stderr)
	}
	var s config.Settings
	if err := json.Unmarshal([]byte(h.read(t, ".dbdiagram/settings.json")), &s); err != nil {
		t.Fatal(err)
	}
	if s != (config.Settings{SourceID: "src-1", Entry: "db/schema.dbml", Format: "dbml"}) {
		t.Fatalf("settings = %+v", s)
	}
	if !strings.Contains(h.read(t, ".dbdiagram/.gitignore"), "state.json") {
		t.Fatal("state.json not gitignored")
	}
	if code := h.run("init", "--source", "src-2"); code != 1 || !strings.Contains(h.stderr.String(), "--force") {
		t.Fatalf("re-init without --force: %d %s", code, h.stderr)
	}
	if code := h.run("init", "--source", "src-2", "--entry", "schema.rb", "--force"); code != 0 {
		t.Fatalf("re-init --force: %d %s", code, h.stderr)
	}
	if !strings.Contains(h.read(t, ".dbdiagram/settings.json"), `"schemarb"`) {
		t.Fatal(".rb entry should default to schemarb")
	}
}

func TestPushPullRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	if code := h.run("init", "--source", "src-1"); code != 0 {
		t.Fatal(h.stderr)
	}
	h.write(t, "schema.dbml", "Table users {\n  id int [pk]\n}\n")

	if code := h.run("push", "--message", "first", "--sha", "abc123", "--ref", "main"); code != 0 {
		t.Fatalf("push: %d %s", code, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), `Pushed to "Main": version 1`) {
		t.Fatalf("push output = %q", h.stdout)
	}
	p := h.api.pushes[0]
	if p["format"] != "dbml" || p["sha"] != "abc123" || p["ref"] != "main" || p["message"] != "first" {
		t.Fatalf("push body = %v", p)
	}
	if code := h.run("push", "--sha", "", "--ref", ""); code != 0 || !strings.Contains(h.stdout.String(), "No changes") {
		t.Fatalf("second push: %d %q", code, h.stdout)
	}
	if _, ok := h.api.pushes[1]["sha"]; ok {
		t.Fatal("--sha '' must send no sha")
	}

	// The server moves on (a web edit): pull brings it down with the layout.
	h.api.dbml, h.api.version = "Table users {\n  id int [pk]\n  email text\n}\n", 2
	if code := h.run("pull"); code != 0 {
		t.Fatalf("pull: %d %s", code, h.stderr)
	}
	if h.read(t, "schema.dbml") != h.api.dbml {
		t.Fatal("pull did not write the DBML")
	}
	if !strings.Contains(h.read(t, "schema.dbdiagram"), `"public.users"`) {
		t.Fatalf("layout companion = %q", h.read(t, "schema.dbdiagram"))
	}
	st, err := config.LoadState(h.env.Cwd)
	if err != nil || st == nil || st.Version != 2 || st.File != "schema.dbml" {
		t.Fatalf("state = %+v %v", st, err)
	}

	// A local edit is protected from the next pull…
	h.write(t, "schema.dbml", "Table local_only {}\n")
	h.api.version = 3
	if code := h.run("pull"); code != 1 || !strings.Contains(h.stderr.String(), "--force") {
		t.Fatalf("dirty pull: %d %s", code, h.stderr)
	}
	if h.read(t, "schema.dbml") != "Table local_only {}\n" {
		t.Fatal("dirty pull overwrote local changes")
	}
	// …until --force.
	if code := h.run("pull", "--force"); code != 0 || h.read(t, "schema.dbml") != h.api.dbml {
		t.Fatalf("forced pull: %d %s", code, h.stderr)
	}
	// A file that was pushed (not pulled) is clean for the next pull too.
	h.write(t, "schema.dbml", "Table pushed {}\n")
	if code := h.run("push"); code != 0 {
		t.Fatal(h.stderr)
	}
	if code := h.run("pull"); code != 0 {
		t.Fatalf("pull after push: %d %s", code, h.stderr)
	}
}

func TestPullNeverPulledRefusesDifferentFile(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	h.run("init", "--source", "src-1")
	h.api.dbml, h.api.version = "Table a {}\n", 1
	h.write(t, "schema.dbml", "Table b {}\n")
	if code := h.run("pull"); code != 1 || !strings.Contains(h.stderr.String(), "never pulled") {
		t.Fatalf("pull: %d %s", code, h.stderr)
	}
	// Identical content is fine without state.
	h.write(t, "schema.dbml", "Table a {}\n")
	if code := h.run("pull"); code != 0 {
		t.Fatalf("pull same: %d %s", code, h.stderr)
	}
}

func TestPullNonDBMLEntryNeedsOut(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	h.run("init", "--source", "src-1", "--entry", "schema.sql", "--format", "mysql")
	h.api.dbml, h.api.version = "Table a {}\n", 1
	if code := h.run("pull"); code != 1 || !strings.Contains(h.stderr.String(), "--out") {
		t.Fatalf("pull: %d %s", code, h.stderr)
	}
	if code := h.run("pull", "--out", "schema.dbml"); code != 0 || h.read(t, "schema.dbml") != "Table a {}\n" {
		t.Fatalf("pull --out: %d %s", code, h.stderr)
	}
}

func TestPushStdinAndFormat(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	h.env.Stdin = strings.NewReader("CREATE TABLE a (id int primary key);")
	if code := h.run("push", "--stdin", "--source", "src-1", "--format", "mysql", "--sha", "", "--ref", ""); code != 0 {
		t.Fatalf("push --stdin: %d %s", code, h.stderr)
	}
	if h.api.pushes[0]["format"] != "mysql" {
		t.Fatalf("format = %v", h.api.pushes[0]["format"])
	}
	h.env.Stdin = strings.NewReader("   \n")
	if code := h.run("push", "--stdin", "--source", "src-1"); code != 1 || !strings.Contains(h.stderr.String(), "empty") {
		t.Fatalf("empty stdin: %d %s", code, h.stderr)
	}
}

func TestPushSizeLimitIsClientSide(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	h.run("init", "--source", "src-1")
	h.write(t, "schema.dbml", strings.Repeat("a", config.MAX_DBML_BYTES+1))
	before := h.api.requests
	if code := h.run("push"); code != 1 || !strings.Contains(h.stderr.String(), "at most 1.00 MiB") {
		t.Fatalf("oversized push: %d %s", code, h.stderr)
	}
	h.env.Stdin = strings.NewReader(strings.Repeat("a", config.MAX_DBML_BYTES+10))
	if code := h.run("push", "--stdin"); code != 1 {
		t.Fatalf("oversized stdin: %d", code)
	}
	if h.api.requests != before {
		t.Fatal("an oversized push reached the API")
	}
}

func TestPushErrors(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	h.run("init", "--source", "src-1")
	h.write(t, "schema.dbml", "Table a {}\n")

	h.api.paused = true
	if code := h.run("push"); code != 1 || !strings.Contains(h.stderr.String(), "paused") {
		t.Fatalf("paused: %d %s", code, h.stderr)
	}
	h.api.paused = false

	h.api.next429 = 1
	if code := h.run("push"); code != 0 {
		t.Fatalf("429 then success: %d %s", code, h.stderr)
	}
	if len(h.slept) != 1 || h.slept[0] != 2*time.Second {
		t.Fatalf("slept = %v, want [2s]", h.slept)
	}

	h.api.next429 = 2
	if code := h.run("push"); code != 1 || !strings.Contains(h.stderr.String(), "rate limited") {
		t.Fatalf("429 twice: %d %s", code, h.stderr)
	}

	h.vars[config.ENV_TOKEN] = "dbd_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	if code := h.run("push"); code != 1 || !strings.Contains(h.stderr.String(), "authentication failed") {
		t.Fatalf("bad token: %d %s", code, h.stderr)
	}
	if strings.Contains(h.stderr.String(), "ZZZZZZZZZZZZZZZ") {
		t.Fatal("error output leaks the token")
	}
	delete(h.vars, config.ENV_TOKEN)

	if code := h.run("push", "--source", "other"); code != 1 || !strings.Contains(h.stderr.String(), "source not found") {
		t.Fatalf("unknown source: %d %s", code, h.stderr)
	}
	if code := h.run("push", "--sha", "not a sha!"); code != 1 || !strings.Contains(h.stderr.String(), "--sha") {
		t.Fatalf("bad sha: %d %s", code, h.stderr)
	}
}

func TestNotLoggedIn(t *testing.T) {
	h := newHarness(t)
	h.run("init", "--source", "src-1")
	h.write(t, "schema.dbml", "Table a {}\n")
	if code := h.run("push"); code != 1 || !strings.Contains(h.stderr.String(), "not logged in") {
		t.Fatalf("push: %d %s", code, h.stderr)
	}
}

func TestVerbosePushNeverPrintsToken(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	h.run("init", "--source", "src-1")
	h.write(t, "schema.dbml", "Table a {}\n")
	if code := h.run("push", "--verbose"); code != 0 {
		t.Fatal(h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "PUT /v1/sources/src-1/push → 200") {
		t.Fatalf("verbose = %q", h.stderr)
	}
	if strings.Contains(h.stderr.String()+h.stdout.String(), testToken) {
		t.Fatal("verbose output leaks the token")
	}
}

func TestPushInspectSQLite(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	dbPath := filepath.Join(t.TempDir(), "app.db")
	if code := runSQL(t, dbPath,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL)`,
		`CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id))`); code != nil {
		t.Fatal(code)
	}
	if code := h.run("push", "--inspect", "--driver", "sqlite", "--database", dbPath, "--source", "src-1", "--sha", "", "--ref", ""); code != 0 {
		t.Fatalf("push --inspect: %d %s", code, h.stderr)
	}
	got, _ := h.api.pushes[0]["dbml"].(string)
	if !strings.Contains(got, "Table users") || !strings.Contains(got, "Ref") {
		t.Fatalf("pushed DBML = %q", got)
	}
	// inspect | push --stdin
	if code := h.run("inspect", "--driver", "sqlite", "--database", dbPath); code != 0 {
		t.Fatal(h.stderr)
	}
	piped := h.stdout.String()
	h.env.Stdin = strings.NewReader(piped)
	if code := h.run("push", "--stdin", "--source", "src-1"); code != 0 || !strings.Contains(h.stdout.String(), "No changes") {
		t.Fatalf("piped push: %d %q %s", code, h.stdout, h.stderr)
	}
}

func TestValidateAPI(t *testing.T) {
	cases := map[string]bool{
		"https://dbdiagram-api.appleby.cloud":  true,
		"https://dbdiagram-api.appleby.cloud/": true,
		"http://localhost:8060":                true,
		"http://127.0.0.1:1234":                true,
		"http://dbdiagram-api.appleby.cloud":   false,
		"https://user:pw@host":                 false,
		"https://host/path":                    false,
		"ftp://host":                           false,
		"not a url":                            false,
	}
	for in, ok := range cases {
		if err := ValidateAPI(in); (err == nil) != ok {
			t.Errorf("ValidateAPI(%q) = %v, want ok=%v", in, err, ok)
		}
	}
}

func TestResolveGitAuto(t *testing.T) {
	h := newHarness(t)
	h.vars["GITHUB_SHA"] = "0123456789abcdef"
	h.vars["GITHUB_REF_NAME"] = "feature/x"
	sha, ref, err := resolveGit(h.env, t.TempDir(), "auto", "auto") // not a repo → CI env
	if err != nil || sha != "0123456789abcdef" || ref != "feature/x" {
		t.Fatalf("resolveGit = %q %q %v", sha, ref, err)
	}
	if _, _, err := resolveGit(h.env, t.TempDir(), "x y", ""); err == nil {
		t.Fatal("invalid explicit sha accepted")
	}
	if _, _, err := resolveGit(h.env, t.TempDir(), "", strings.Repeat("r", 256)); err == nil {
		t.Fatal("overlong ref accepted")
	}
}
