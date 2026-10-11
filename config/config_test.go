package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testToken = "dbd_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"

func TestValidToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"valid", testToken, true},
		{"short", "dbd_abc", false},
		{"wrong prefix", "frt_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", false},
		{"bad char", "dbd_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO/Q", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ValidToken(c.in); got != c.want {
				t.Fatalf("ValidToken(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestMaskToken(t *testing.T) {
	got := MaskToken(testToken)
	if got != "dbd_abcdefgh…" {
		t.Fatalf("MaskToken = %q", got)
	}
	if strings.Contains(got, testToken[12:]) {
		t.Fatal("mask leaks the token body")
	}
	if MaskToken("short") != "****" {
		t.Fatal("short tokens must be fully masked")
	}
}

func TestCredentialsRoundTripAndPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dbdiagram")
	if c, err := LoadCredentials(dir); err != nil || c != nil {
		t.Fatalf("missing file: got %v, %v", c, err)
	}
	if err := SaveCredentials(dir, Credentials{Token: testToken, API: "https://x.example"}); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCredentials(dir)
	if err != nil || c == nil || c.Token != testToken || c.API != "https://x.example" {
		t.Fatalf("round trip: %+v, %v", c, err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, CREDENTIALS_FILE))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("credentials mode = %o, want 600", st.Mode().Perm())
		}
		dst, _ := os.Stat(dir)
		if dst.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %o, want 700", dst.Mode().Perm())
		}
		if CredentialsInsecure(dir) {
			t.Fatal("0600 file reported insecure")
		}
		if err := os.Chmod(filepath.Join(dir, CREDENTIALS_FILE), 0o644); err != nil {
			t.Fatal(err)
		}
		if !CredentialsInsecure(dir) {
			t.Fatal("0644 file not reported insecure")
		}
		// Saving again restores 0600 even over a loosened file.
		if err := SaveCredentials(dir, Credentials{Token: testToken}); err != nil {
			t.Fatal(err)
		}
		st, _ = os.Stat(filepath.Join(dir, CREDENTIALS_FILE))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("re-save mode = %o, want 600", st.Mode().Perm())
		}
	}
	removed, err := DeleteCredentials(dir)
	if err != nil || !removed {
		t.Fatalf("delete: %v %v", removed, err)
	}
	removed, err = DeleteCredentials(dir)
	if err != nil || removed {
		t.Fatalf("second delete: %v %v", removed, err)
	}
}

func TestDir(t *testing.T) {
	got, err := Dir(func(k string) string {
		if k == ENV_CONFIG_DIR {
			return "/custom"
		}
		return ""
	})
	if err != nil || got != "/custom" {
		t.Fatalf("override: %q %v", got, err)
	}
	got, err = Dir(func(string) string { return "" })
	if err != nil || filepath.Base(got) != "dbdiagram" {
		t.Fatalf("default: %q %v", got, err)
	}
}

func TestSettingsValidate(t *testing.T) {
	ok := Settings{SourceID: "3f1c2a9e-0000-4000-8000-000000000001", Entry: "db/schema.dbml", Format: "dbml"}
	cases := []struct {
		name    string
		mut     func(*Settings)
		wantErr string
	}{
		{"valid", func(*Settings) {}, ""},
		{"empty source", func(s *Settings) { s.SourceID = "" }, "source_id"},
		{"path in source", func(s *Settings) { s.SourceID = "../tokens" }, "source_id"},
		{"no entry", func(s *Settings) { s.Entry = "" }, "entry is required"},
		{"absolute entry", func(s *Settings) { s.Entry = "/etc/passwd" }, "relative"},
		{"escaping entry", func(s *Settings) { s.Entry = "../outside.dbml" }, "inside"},
		{"dot entry", func(s *Settings) { s.Entry = "." }, "inside"},
		{"bad format", func(s *Settings) { s.Format = "sqlite" }, "format"},
		{"schemarb", func(s *Settings) { s.Format = "schemarb" }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := ok
			c.mut(&s)
			err := s.Validate()
			if c.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("error = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func TestSettingsSaveLoadFindRoot(t *testing.T) {
	root := t.TempDir()
	s := Settings{SourceID: "abc-123", Entry: "schema.dbml", Format: "dbml"}
	if err := SaveSettings(root, s); err != nil {
		t.Fatal(err)
	}
	gi, err := os.ReadFile(filepath.Join(root, DIR_NAME, GITIGNORE_FILE))
	if err != nil || !strings.Contains(string(gi), STATE_FILE) {
		t.Fatalf(".gitignore must ignore state.json: %q %v", gi, err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, DIR_NAME, SETTINGS_FILE))
	if strings.Contains(string(raw), "dbd_") {
		t.Fatal("settings must never carry a token")
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	found, err := FindRoot(sub)
	if err != nil {
		t.Fatal(err)
	}
	if rs, _ := filepath.EvalSymlinks(found); rs != mustEval(t, root) {
		t.Fatalf("FindRoot = %q, want %q", found, root)
	}
	got, err := LoadSettings(found)
	if err != nil || *got != s {
		t.Fatalf("LoadSettings = %+v, %v", got, err)
	}
	if _, err := FindRoot(t.TempDir()); err != ErrNoSettings {
		t.Fatalf("no settings: err = %v", err)
	}
}

func TestLoadSettingsDefaultsFormatAndValidates(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, DIR_NAME), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(root, DIR_NAME, SETTINGS_FILE), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"source_id":"abc","entry":"schema.dbml"}`)
	s, err := LoadSettings(root)
	if err != nil || s.Format != "dbml" {
		t.Fatalf("default format: %+v %v", s, err)
	}
	write(`{"source_id":"abc","entry":"../../etc/passwd"}`)
	if _, err := LoadSettings(root); err == nil {
		t.Fatal("an escaping entry must be refused")
	}
	write(`{not json`)
	if _, err := LoadSettings(root); err == nil {
		t.Fatal("bad JSON must be refused")
	}
}

func TestCheckSize(t *testing.T) {
	cases := []struct {
		format string
		n      int
		ok     bool
	}{
		{"dbml", MAX_DBML_BYTES, true},
		{"dbml", MAX_DBML_BYTES + 1, false},
		{"", MAX_DBML_BYTES + 1, false},
		{"mysql", MAX_DBML_BYTES + 1, true},
		{"postgres", MAX_IMPORT_BYTES, true},
		{"schemarb", MAX_IMPORT_BYTES + 1, false},
	}
	for _, c := range cases {
		err := CheckSize(c.format, c.n)
		if (err == nil) != c.ok {
			t.Errorf("CheckSize(%q, %d) = %v, want ok=%v", c.format, c.n, err, c.ok)
		}
	}
}

func TestLayoutPath(t *testing.T) {
	cases := map[string]string{
		"schema.dbml":        "schema.dbdiagram",
		"db/main.dbml":       "db/main.dbdiagram",
		"noext":              "noext.dbdiagram",
		"a.b/schema.v2.dbml": "a.b/schema.v2.dbdiagram",
	}
	for in, want := range cases {
		if got := LayoutPath(in); got != want {
			t.Errorf("LayoutPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	root := t.TempDir()
	if s, err := LoadState(root); err != nil || s != nil {
		t.Fatalf("missing state: %v %v", s, err)
	}
	want := State{SourceID: "abc", Version: 7, File: "schema.dbml", ContentSHA256: HashContent([]byte("x"))}
	if err := SaveState(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(root)
	if err != nil || got.Version != 7 || got.ContentSHA256 != want.ContentSHA256 || got.File != "schema.dbml" {
		t.Fatalf("state = %+v %v", got, err)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestValidateEntryMetadata(t *testing.T) {
	bad := []string{".git/config", "x/.git/HEAD", ".github/workflows/ci.yml", ".gitmodules", ".gitignore", ".dbdiagram/settings.json", "a/.dbdiagram/x", "schema.dbdiagram", "SCHEMA.DBDIAGRAM", "/abs.dbml", "../x.dbml", ".."}
	for _, e := range bad {
		if ValidateEntry(e) == nil {
			t.Errorf("ValidateEntry(%q) accepted", e)
		}
	}
	for _, e := range []string{"schema.dbml", "db/schema.sql", "git/schema.dbml", "my.gitlike/x.dbml"} {
		if err := ValidateEntry(e); err != nil {
			t.Errorf("ValidateEntry(%q) = %v", e, err)
		}
	}
}

func TestRelInside(t *testing.T) {
	cases := []struct {
		base, target, rel string
		ok                bool
	}{
		{"/p", "/p/a/b", "a/b", true},
		{"/p", "/p/..x", "..x", true},
		{"/p", "/p", ".", true},
		{"/p", "/q", "", false},
		{"/p", "/p/../q", "", false},
		{"/p", "/", "", false},
	}
	for _, c := range cases {
		rel, ok := RelInside(c.base, c.target)
		if ok != c.ok || rel != c.rel {
			t.Errorf("RelInside(%q,%q) = %q,%v", c.base, c.target, rel, ok)
		}
	}
}

func TestProjectReadRegularBounded(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.WriteFile("a/b.txt", []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := p.ReadRegular("a/b.txt", 5); err != nil || string(b) != "12345" {
		t.Fatalf("read = %q %v", b, err)
	}
	if _, err := p.ReadRegular("a/b.txt", 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over limit: %v", err)
	}
	if _, err := p.ReadRegular("a", 10); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("dir: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "a", "b.txt"), filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadRegular("l", 10); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlink read: %v", err)
	}
	if err := p.WriteFile("l", []byte("x"), 0o644); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlink write: %v", err)
	}
	if _, err := p.ReadRegular("../escape", 10); err == nil {
		t.Fatal("escape read")
	}
	if err := p.WriteFile("../escape", []byte("x"), 0o644); err == nil {
		t.Fatal("escape write")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "a"))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

func TestSaveCredentialsTightensDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes")
	}
	dir := filepath.Join(t.TempDir(), "dbdiagram")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if !ConfigDirWritable(dir) {
		t.Fatal("0777 dir not reported writable")
	}
	if err := SaveCredentials(dir, Credentials{Token: testToken}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(dir)
	if st.Mode().Perm() != 0o700 || ConfigDirWritable(dir) {
		t.Fatalf("dir mode = %o", st.Mode().Perm())
	}
	// A symlinked credentials file is not followed.
	target := filepath.Join(t.TempDir(), "target.json")
	_ = os.WriteFile(target, []byte(`{"token":"`+testToken+`","api":"https://evil.example"}`), 0o600)
	_ = os.Remove(filepath.Join(dir, CREDENTIALS_FILE))
	_ = os.Symlink(target, filepath.Join(dir, CREDENTIALS_FILE))
	if _, err := LoadCredentials(dir); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlinked credentials: %v", err)
	}
}

func TestMetadataComponent(t *testing.T) {
	cases := []struct {
		rel     string
		windows bool
		want    string
	}{
		{"a/b.dbml", false, ""},
		{".git/config", false, ".git"},
		{"x/.GitHub/y", false, ".GitHub"},
		{".dbdiagram/state.json", false, ".dbdiagram"},
		{".DBDIAGRAM./x", false, ".DBDIAGRAM."},
		{"GIT~1/config", true, "GIT~1"},
		{"GIT~1/config", false, ""},
		{"DBDIAG~2/state.json", true, "DBDIAG~2"},
	}
	for _, c := range cases {
		if got := metadataComponent(c.rel, c.windows); got != c.want {
			t.Errorf("metadataComponent(%q, %v) = %q, want %q", c.rel, c.windows, got, c.want)
		}
	}
}

func TestSafeTextAndControlEntries(t *testing.T) {
	if got := SafeText("a\x1b[31mb\x07\u009bc"); got != `a\x1b[31mb\x07\x9bc` {
		t.Fatalf("SafeText = %q", got)
	}
	if SafeText("plain é") != "plain é" {
		t.Fatal("SafeText changed printable text")
	}
	if ValidateEntry("\x1b]0;x\x07.dbml") == nil || ValidateEntry("a\nb.dbml") == nil {
		t.Fatal("control characters accepted in entry")
	}
}

func TestProjectRefusesSymlinkedDirectoryComponents(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "real", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "real", "sub", "f"), []byte("x"), 0o644)
	_ = os.Symlink("real", filepath.Join(dir, "alias"))
	p, err := OpenProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.ReadRegular("alias/sub/f", 10); !errors.Is(err, ErrSymlink) {
		t.Fatalf("read through alias: %v", err)
	}
	if err := p.WriteFile("alias/sub/g", []byte("x"), 0o644); !errors.Is(err, ErrSymlink) {
		t.Fatalf("write through alias: %v", err)
	}
	if err := p.WriteFile("alias/new/g", []byte("x"), 0o644); !errors.Is(err, ErrSymlink) {
		t.Fatalf("mkdir through alias: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "real", "new")); err == nil {
		t.Fatal("directory created through a symlink")
	}
	if b, err := p.ReadRegular("real/sub/f", 10); err != nil || string(b) != "x" {
		t.Fatalf("plain read: %q %v", b, err)
	}
}
