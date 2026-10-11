package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	// DIR_NAME is the per-project directory, at the project root.
	DIR_NAME       = ".dbdiagram"
	SETTINGS_FILE  = "settings.json"
	STATE_FILE     = "state.json"
	GITIGNORE_FILE = ".gitignore"

	// LAYOUT_EXT is the extension of the layout companion written next to
	// the entry file by pull (schema.dbml → schema.dbdiagram).
	LAYOUT_EXT = ".dbdiagram"
)

// Format limits, mirroring dbdiagram-api's push contract.
const (
	MAX_DBML_BYTES   = 1 << 20 // 1 MiB of DBML
	MAX_IMPORT_BYTES = 4 << 20 // 4 MiB of SQL / schema.rb
)

// FORMATS are the push formats the API accepts.
var FORMATS = []string{"dbml", "mysql", "postgres", "mssql", "schemarb"}

// ValidFormat reports whether f is a push format.
func ValidFormat(f string) bool {
	for _, x := range FORMATS {
		if f == x {
			return true
		}
	}
	return false
}

// MaxBytes is the client-side size limit for content in format f.
func MaxBytes(format string) int {
	if format == "" || format == "dbml" {
		return MAX_DBML_BYTES
	}
	return MAX_IMPORT_BYTES
}

// CheckSize refuses content larger than the API accepts for format, before
// anything is sent.
func CheckSize(format string, n int) error {
	if max := MaxBytes(format); n > max {
		what := "DBML"
		if format != "" && format != "dbml" {
			what = format + " input"
		}
		return fmt.Errorf("%s is %s; the API accepts at most %s", what, humanBytes(n), humanBytes(max))
	}
	return nil
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// sourceIDPattern keeps the id path-safe: it is interpolated into a URL path.
var sourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// ValidSourceID reports whether id can be a source id.
func ValidSourceID(id string) bool { return sourceIDPattern.MatchString(id) }

// Settings is .dbdiagram/settings.json: committable, never a secret.
type Settings struct {
	SourceID string `json:"source_id"`
	Entry    string `json:"entry"`
	Format   string `json:"format"`
}

// ErrNoSettings means no .dbdiagram/settings.json was found.
var ErrNoSettings = errors.New("no .dbdiagram/settings.json found here or in any parent directory (run `dbdiagram init`)")

// Validate checks every field.
func (s Settings) Validate() error {
	if !ValidSourceID(s.SourceID) {
		return fmt.Errorf("source_id %q is not a valid source id", s.SourceID)
	}
	if err := ValidateEntry(s.Entry); err != nil {
		return err
	}
	if !ValidFormat(s.Format) {
		return fmt.Errorf("format %q must be one of %s", s.Format, strings.Join(FORMATS, ", "))
	}
	return nil
}

// MAX_SETTINGS_BYTES bounds .dbdiagram/settings.json and state.json reads.
const MAX_SETTINGS_BYTES = 64 << 10

// ValidateEntry requires a relative path that stays inside the project and
// is not repository or CLI metadata: no component starting with ".git"
// (.git, .github, .gitmodules…), no ".dbdiagram" directory, and not a
// layout companion (*.dbdiagram).
func ValidateEntry(entry string) error {
	if entry == "" {
		return errors.New("entry is required")
	}
	if hasControl(entry) {
		return fmt.Errorf("entry %q must not contain control characters", entry)
	}
	if filepath.IsAbs(entry) || strings.HasPrefix(entry, "/") || strings.HasPrefix(entry, `\`) || filepath.VolumeName(entry) != "" {
		return fmt.Errorf("entry %q must be relative to the project root", entry)
	}
	clean := filepath.Clean(filepath.FromSlash(entry))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("entry %q must stay inside the project root", entry)
	}
	if bad := metadataComponent(filepath.ToSlash(clean), runtime.GOOS == "windows"); bad != "" {
		return fmt.Errorf("entry %q must not be inside %q", entry, bad)
	}
	if strings.HasSuffix(strings.ToLower(clean), LAYOUT_EXT) {
		return fmt.Errorf("entry %q must not be a %s layout file", entry, LAYOUT_EXT)
	}
	return nil
}

// shortName matches a Windows 8.3 short-name component (GIT~1, DBDIAG~2).
var shortName = regexp.MustCompile(`~[0-9]`)

// metadataComponent returns the first component of rel (slash-separated)
// that is repository or CLI metadata: starts with ".git" or equals
// ".dbdiagram" (case-insensitive), or — on Windows — looks like an 8.3
// short name, which could alias either. "" when none.
func metadataComponent(rel string, windows bool) string {
	for _, part := range strings.Split(rel, "/") {
		l := strings.ToLower(strings.TrimRight(part, ". "))
		if strings.HasPrefix(l, ".git") || l == DIR_NAME || (windows && shortName.MatchString(part)) {
			return part
		}
	}
	return ""
}

// hasControl reports C0/C1 control characters and DEL.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}

// SafeText escapes C0/C1 control characters (terminal escape sequences) in
// text that is about to be printed; everything else is kept as is.
func SafeText(s string) string {
	if !hasControl(s) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			fmt.Fprintf(&b, "\\x%02x", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FindRoot walks up from start to the directory containing
// .dbdiagram/settings.json.
func FindRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, DIR_NAME, SETTINGS_FILE)); err == nil && !st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNoSettings
		}
		dir = parent
	}
}

// readMeta reads a .dbdiagram/ file through the project root.
func readMeta(root, name string) ([]byte, error) {
	p, err := OpenProject(root)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return p.ReadRegular(DIR_NAME+"/"+name, MAX_SETTINGS_BYTES)
}

// writeMeta writes a .dbdiagram/ file through the project root.
func writeMeta(root, name string, b []byte) error {
	p, err := OpenProject(root)
	if err != nil {
		return err
	}
	defer p.Close()
	return p.WriteFile(DIR_NAME+"/"+name, b, 0o644)
}

// LoadSettings reads root/.dbdiagram/settings.json and validates it.
func LoadSettings(root string) (*Settings, error) {
	b, err := readMeta(root, SETTINGS_FILE)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoSettings
	}
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse .dbdiagram/settings.json: %w", err)
	}
	if s.Format == "" {
		s.Format = "dbml"
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf(".dbdiagram/settings.json: %w", err)
	}
	return &s, nil
}

// SaveSettings writes root/.dbdiagram/settings.json (0644) and a
// .dbdiagram/.gitignore that keeps state.json out of git.
func SaveSettings(root string, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// .gitignore first: settings.json is what marks a project, so it is
	// written last and a refusal never leaves a half-initialised project.
	if err := writeMeta(root, GITIGNORE_FILE, []byte("# local pull state; never commit\n"+STATE_FILE+"\n")); err != nil {
		return err
	}
	return writeMeta(root, SETTINGS_FILE, append(b, '\n'))
}

// LayoutPath is the layout companion of an entry: schema.dbml → schema.dbdiagram.
func LayoutPath(entry string) string {
	return strings.TrimSuffix(entry, filepath.Ext(entry)) + LAYOUT_EXT
}

// State is .dbdiagram/state.json: what the working copy last matched on the
// server. Local only (gitignored).
type State struct {
	SourceID string `json:"source_id"`
	// Version is the diagram version last pulled or pushed.
	Version int `json:"version"`
	// File is the path (relative to the root) the content was written to.
	File string `json:"file"`
	// ContentSHA256 is the hash of File's content as last pulled or pushed;
	// a different hash on disk means local, unpushed changes.
	ContentSHA256 string    `json:"content_sha256"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// LoadState reads root/.dbdiagram/state.json. A missing file is (nil, nil).
func LoadState(root string) (*State, error) {
	b, err := readMeta(root, STATE_FILE)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse .dbdiagram/state.json: %w", err)
	}
	return &s, nil
}

// SaveState writes root/.dbdiagram/state.json.
func SaveState(root string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeMeta(root, STATE_FILE, append(b, '\n'))
}

// HashContent is the state's content hash.
func HashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
