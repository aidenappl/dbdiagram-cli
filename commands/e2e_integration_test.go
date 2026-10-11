//go:build integration

package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aidenappl/dbdiagram-cli/config"
	"github.com/aidenappl/dbdiagram-cli/testenv"
)

// TestEndToEndAgainstRealAPI runs the real dbdiagram-api (built from
// ../dbdiagram-api, DBD_ENVIRONMENT=development, its own MariaDB 11.4
// container), creates a sync token through the API, and drives the CLI:
// push → pull round trip, skip, SQL import, scope and pause refusals.
// Skipped (with the reason) when the API repo is missing or cannot start.
func TestEndToEndAgainstRealAPI(t *testing.T) {
	apiDir := os.Getenv("DBDIAGRAM_API_DIR")
	if apiDir == "" {
		apiDir = filepath.Join("..", "..", "dbdiagram-api")
	}
	if _, err := os.Stat(filepath.Join(apiDir, "go.mod")); err != nil {
		t.Skipf("dbdiagram-api not found at %s (set DBDIAGRAM_API_DIR): %v", apiDir, err)
	}
	ctx := context.Background()

	bin := filepath.Join(t.TempDir(), "dbdiagram-api")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	build.Dir = apiDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build dbdiagram-api (%v):\n%s", err, out)
	}

	db, err := testenv.StartMariaDB(ctx, "dbdiagram")
	t.Cleanup(db.Terminate)
	if err != nil {
		t.Skipf("cannot start MariaDB for the API: %v", err)
	}

	port := freePort(t)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	const adminEmail, adminPassword = "e2e-admin@example.com", "E2e-admin-pass-8842x"
	api := exec.Command(bin)
	api.Dir = apiDir
	api.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"DBD_ENVIRONMENT=development",
		"DBD_PORT=" + strconv.Itoa(port),
		fmt.Sprintf("DBD_DB_DSN=root:%s@tcp(%s:%d)/dbdiagram?parseTime=true", testenv.ROOT_PASSWORD, db.Host, db.Port),
		"DBD_PUBLIC_URL=" + base,
		"DBD_WEB_URL=http://localhost:3000",
		"DBD_ADMIN_EMAIL=" + adminEmail,
		"DBD_ADMIN_PASSWORD=" + adminPassword,
	}
	var apiLog lockedBuffer
	api.Stdout, api.Stderr = &apiLog, &apiLog
	if err := api.Start(); err != nil {
		t.Skipf("cannot start dbdiagram-api: %v", err)
	}
	t.Cleanup(func() {
		_ = api.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = api.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = api.Process.Kill()
		}
	})
	if err := waitHealthy(base+"/healthcheck", 90*time.Second); err != nil {
		t.Skipf("dbdiagram-api did not become healthy (%v); log:\n%s", err, apiLog.String())
	}

	// --- set-up through the API as the bootstrap admin (session + CSRF) ---
	s := newSession(t, base)
	s.call(http.MethodPost, "/auth/login", map[string]any{"email": adminEmail, "password": adminPassword}, 200, nil)
	var teams []struct {
		ID       string `json:"id"`
		Personal bool   `json:"personal"`
	}
	s.call(http.MethodGet, "/v1/teams", nil, 200, &teams)
	if len(teams) == 0 {
		t.Fatal("bootstrap admin has no personal team")
	}
	team := teams[0].ID
	var diagram struct {
		ID string `json:"id"`
	}
	s.call(http.MethodPost, "/v1/teams/"+team+"/diagrams", map[string]any{"name": "E2E", "dbml": "", "visibility": "team"}, 201, &diagram)
	var source struct {
		ID string `json:"id"`
	}
	s.call(http.MethodPost, "/v1/teams/"+team+"/diagrams/"+diagram.ID+"/sources", map[string]any{"kind": "cli", "name": "cli"}, 201, &source)
	var tok struct {
		Token string `json:"token"`
	}
	s.call(http.MethodPost, "/v1/teams/"+team+"/tokens", map[string]any{"name": "e2e", "scopes": []string{"sync"}}, 201, &tok)
	var readTok struct {
		Token string `json:"token"`
	}
	s.call(http.MethodPost, "/v1/teams/"+team+"/tokens", map[string]any{"name": "read-only", "scopes": []string{"read"}}, 201, &readTok)
	if !config.ValidToken(tok.Token) || !config.ValidToken(readTok.Token) {
		t.Fatal("API returned malformed tokens")
	}

	// --- the CLI, in-process, against the real API ---
	h := newHarness(t)
	h.vars[config.ENV_API] = base
	if code := h.run("auth", "login", "--token", tok.Token); code != 0 {
		t.Fatalf("login: %s", h.stderr)
	}
	if code := h.run("init", "--source", source.ID); code != 0 {
		t.Fatalf("init: %s", h.stderr)
	}
	schema := "Table users {\n  id int [pk]\n  email varchar [not null]\n}\n\nTable posts {\n  id int [pk]\n  user_id int [ref: > users.id]\n}\n"
	h.write(t, "schema.dbml", schema)
	if code := h.run("push", "--message", "e2e first push", "--sha", "e2e0001", "--ref", "main"); code != 0 {
		t.Fatalf("push: %s\nAPI log:\n%s", h.stderr, apiLog.String())
	}
	if !strings.Contains(h.stdout.String(), "Pushed to \"E2E\"") {
		t.Fatalf("push output = %q", h.stdout)
	}
	if code := h.run("push"); code != 0 || !strings.Contains(h.stdout.String(), "No changes") {
		t.Fatalf("identical push: %d %q %s", code, h.stdout, h.stderr)
	}

	// pull into a fresh checkout: the DBML round-trips byte for byte.
	other := newHarness(t)
	other.vars = h.vars
	other.run("init", "--source", source.ID)
	if code := other.run("pull"); code != 0 {
		t.Fatalf("pull: %s", other.stderr)
	}
	if got := other.read(t, "schema.dbml"); got != schema {
		t.Fatalf("round trip mismatch:\n--- pushed\n%s\n--- pulled\n%s", schema, got)
	}
	var layout map[string]any
	if err := json.Unmarshal([]byte(other.read(t, "schema.dbdiagram")), &layout); err != nil {
		t.Fatalf("layout companion is not JSON: %v", err)
	}
	st, _ := config.LoadState(other.env.Cwd)
	if st == nil || st.Version < 1 {
		t.Fatalf("state = %+v", st)
	}

	// SQL import through --stdin.
	h.env.Stdin = strings.NewReader("CREATE TABLE orgs (id INT PRIMARY KEY, name VARCHAR(100) NOT NULL);")
	if code := h.run("push", "--stdin", "--format", "mysql", "--sha", "", "--ref", ""); code != 0 {
		t.Fatalf("sql push: %s", h.stderr)
	}
	if code := other.run("pull"); code != 0 || !strings.Contains(other.read(t, "schema.dbml"), "orgs") {
		t.Fatalf("pull after SQL push: %s", other.stderr)
	}

	// A token without the sync scope → 403/4003, explained.
	h.vars[config.ENV_TOKEN] = readTok.Token
	if code := h.run("push"); code != 1 || !strings.Contains(h.stderr.String(), "lacks the `sync` scope") {
		t.Fatalf("read-only token push: %d %s", code, h.stderr)
	}
	delete(h.vars, config.ENV_TOKEN)

	// Paused source → 409.
	s.call(http.MethodPatch, "/v1/teams/"+team+"/diagrams/"+diagram.ID+"/sources/"+source.ID, map[string]any{"status": "paused"}, 200, nil)
	h.write(t, "schema.dbml", schema+"\nTable extra {\n  id int\n}\n")
	if code := h.run("push"); code != 1 || !strings.Contains(h.stderr.String(), "paused") {
		t.Fatalf("paused push: %d %s", code, h.stderr)
	}

	// Unknown source → 404, explained.
	if code := h.run("pull", "--source", "00000000-0000-4000-8000-000000000000", "--out", "x.dbml"); code != 1 || !strings.Contains(h.stderr.String(), "source not found") {
		t.Fatalf("unknown source: %d %s", code, h.stderr)
	}
}

// session is a cookie + CSRF client for set-up calls.
type session struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newSession(t *testing.T, base string) *session {
	jar, _ := cookiejar.New(nil)
	return &session{t: t, base: base, c: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

func (s *session) call(method, path string, body any, wantStatus int, out any) {
	s.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.base+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	u, _ := req.URL.Parse(s.base)
	for _, c := range s.c.Jar.Cookies(u) {
		if c.Name == "dbd-csrf" {
			req.Header.Set("X-CSRF-Token", c.Value)
		}
	}
	resp, err := s.c.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		s.t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, wantStatus, raw)
	}
	if out != nil {
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			s.t.Fatal(err)
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			s.t.Fatalf("%s %s data: %v: %s", method, path, err, env.Data)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitHealthy(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return last
}

// lockedBuffer collects the API's output from two goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
