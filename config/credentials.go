// Package config owns everything the CLI keeps on disk: the user's
// credentials (OS config dir, 0600), the committable project settings
// (.dbdiagram/settings.json) and the local, gitignored pull state
// (.dbdiagram/state.json).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
)

const (
	// DEFAULT_API is the production API origin.
	DEFAULT_API = "https://dbdiagram-api.appleby.cloud"

	// ENV_TOKEN / ENV_API override the stored credentials.
	ENV_TOKEN = "DBDIAGRAM_TOKEN"
	ENV_API   = "DBDIAGRAM_API"
	// ENV_CONFIG_DIR overrides the credentials directory (tests, CI sandboxes).
	ENV_CONFIG_DIR = "DBDIAGRAM_CONFIG_DIR"

	CREDENTIALS_FILE = "credentials.json"

	// TOKEN_PREFIX_LEN matches the API's display prefix: "dbd_" + 8 chars.
	TOKEN_PREFIX_LEN = 12
)

// tokenPattern is the API token format: "dbd_" + 43 base64url characters.
var tokenPattern = regexp.MustCompile(`^dbd_[A-Za-z0-9_-]{43}$`)

// ValidToken reports whether t has the API token shape.
func ValidToken(t string) bool { return tokenPattern.MatchString(t) }

// MaskToken returns the display prefix of a token (never the whole token).
func MaskToken(t string) string {
	if len(t) < TOKEN_PREFIX_LEN {
		return "****"
	}
	return t[:TOKEN_PREFIX_LEN] + "…"
}

// Credentials is the stored login.
type Credentials struct {
	Token string `json:"token"`
	API   string `json:"api,omitempty"`
}

// Dir returns the credentials directory: $DBDIAGRAM_CONFIG_DIR, else
// os.UserConfigDir()/dbdiagram.
func Dir(getenv func(string) string) (string, error) {
	if d := getenv(ENV_CONFIG_DIR); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the user config directory: %w", err)
	}
	return filepath.Join(base, "dbdiagram"), nil
}

// LoadCredentials reads dir/credentials.json. A missing file is (nil, nil).
func LoadCredentials(dir string) (*Credentials, error) {
	b, err := ReadExplicit(filepath.Join(dir, CREDENTIALS_FILE), MAX_SETTINGS_BYTES)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	return &c, nil
}

// CredentialsInsecure reports whether the credentials file is readable by
// anyone but its owner (always false on Windows, where modes are synthetic).
func CredentialsInsecure(dir string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, CREDENTIALS_FILE))
	return err == nil && st.Mode().Perm()&0o077 != 0
}

// ConfigDirWritable reports whether the credentials directory is writable
// by group or others (someone else could replace credentials.json).
func ConfigDirWritable(dir string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	st, err := os.Stat(dir)
	return err == nil && st.Mode().Perm()&0o022 != 0
}

// SaveCredentials writes dir/credentials.json with mode 0600 (directory
// 0700), atomically.
func SaveCredentials(dir string, c Credentials) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone: tighten it.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("restrict config directory: %w", err)
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	p, err := OpenProject(dir)
	if err != nil {
		return err
	}
	defer p.Close()
	// Temp file created 0600 inside the dir's root, then renamed (removed
	// through the root on failure); never a chmod by path afterwards.
	return p.WriteFile(CREDENTIALS_FILE, append(b, '\n'), 0o600)
}

// DeleteCredentials removes the stored login. A missing file is not an error.
func DeleteCredentials(dir string) (bool, error) {
	err := os.Remove(filepath.Join(dir, CREDENTIALS_FILE))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove credentials: %w", err)
	}
	return true, nil
}
