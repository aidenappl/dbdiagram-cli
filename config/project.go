package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// Every read and write of a project file goes through an *os.Root opened on
// the project directory, so a path from a (possibly hostile, committed)
// settings.json — or a symlinked directory inside the checkout such as
// `home -> ~` — can never reach a file outside the project. On top of the
// root's confinement:
//
//   - the final component must not be a symlink (read or write), even one
//     that stays inside the project;
//   - reads require a regular file and are bounded (so /dev/zero, a FIFO or a
//     multi-GB file never reach memory);
//   - writes go to a temp file created inside the root, then Rename.

// ErrSymlink is returned for a project path whose final component is a symlink.
var ErrSymlink = errors.New("is a symbolic link; refusing to follow it")

// ErrNotRegular is returned for a project path that is not a regular file.
var ErrNotRegular = errors.New("is not a regular file")

// ErrTooLarge is returned when a bounded read exceeds its limit.
var ErrTooLarge = errors.New("is larger than the limit")

// Project is a directory opened as an *os.Root.
type Project struct {
	Dir  string
	root *os.Root
}

// OpenProject opens dir as a root.
func OpenProject(dir string) (*Project, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Project{Dir: dir, root: r}, nil
}

// Close releases the root.
func (p *Project) Close() error { return p.root.Close() }

// slash converts a root-relative path to the form os.Root expects.
func slash(rel string) string { return filepath.FromSlash(path.Clean(filepath.ToSlash(rel))) }

// Lstat stats rel without following a final symlink.
func (p *Project) Lstat(rel string) (fs.FileInfo, error) { return p.root.Lstat(slash(rel)) }

// ReadRegular reads rel, which must be a regular file (not a symlink) of at
// most max bytes. A missing file returns an error wrapping fs.ErrNotExist.
func (p *Project) ReadRegular(rel string, max int64) ([]byte, error) {
	name := slash(rel)
	if err := p.noSymlinkDirs(name, rel); err != nil {
		return nil, err
	}
	li, err := p.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q %w", rel, ErrSymlink)
	}
	if !li.Mode().IsRegular() {
		return nil, fmt.Errorf("%q %w", rel, ErrNotRegular)
	}
	if li.Size() > max {
		return nil, fmt.Errorf("%q is %d bytes: %w (%d bytes)", rel, li.Size(), ErrTooLarge, max)
	}
	f, err := p.root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The path may have been swapped between Lstat and Open: insist that
	// what was opened is the regular file that was checked.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := p.windowsRealPathCheck(name, rel); err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		return nil, fmt.Errorf("%q changed while it was being opened", rel)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%q: %w (%d bytes)", rel, ErrTooLarge, max)
	}
	return b, nil
}

// WriteFile atomically writes rel inside the project (parents created
// inside the root). An existing destination must be a regular file, never a
// symlink.
func (p *Project) WriteFile(rel string, b []byte, perm os.FileMode) error {
	name := slash(rel)
	if err := p.noSymlinkDirs(name, rel); err != nil {
		return err
	}
	if li, err := p.root.Lstat(name); err == nil {
		if li.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%q %w", rel, ErrSymlink)
		}
		if !li.Mode().IsRegular() {
			return fmt.Errorf("%q %w", rel, ErrNotRegular)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if dir := filepath.Dir(name); dir != "." {
		if err := p.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		// Re-check after creating parents (a component may have appeared).
		if err := p.noSymlinkDirs(name, rel); err != nil {
			return err
		}
	}
	if err := p.windowsRealPathCheck(name, rel); err != nil {
		return err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(name), "."+filepath.Base(name)+".tmp-"+hex.EncodeToString(rnd[:]))
	f, err := p.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = p.root.Rename(tmp, name)
	}
	if werr != nil {
		_ = p.root.Remove(tmp)
	}
	return werr
}

// noSymlinkDirs refuses a symlink at any directory component of name
// (checked through the root with Lstat). A symlink that stays inside the
// project would otherwise reach repository or CLI metadata under another
// name: `g -> .git` + entry g/config, or `m -> .dbdiagram` + m/state.json.
func (p *Project) noSymlinkDirs(name, rel string) error {
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := 1; i < len(parts); i++ {
		prefix := filepath.FromSlash(strings.Join(parts[:i], "/"))
		li, err := p.root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // nothing deeper exists yet
		}
		if err != nil {
			return err
		}
		if li.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%q: directory %q %w", rel, filepath.ToSlash(prefix), ErrSymlink)
		}
		if !li.IsDir() {
			return fmt.Errorf("%q: %q is not a directory", rel, filepath.ToSlash(prefix))
		}
	}
	return nil
}

// windowsRealPathCheck resolves the real (long-name) path on Windows, where
// 8.3 short names (GIT~1) and case-insensitivity could disguise .git or
// .dbdiagram, and refuses it if any real component is metadata.
func (p *Project) windowsRealPathCheck(name, rel string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	base, err := filepath.EvalSymlinks(p.Dir)
	if err != nil {
		return err
	}
	target := filepath.Join(p.Dir, name)
	if _, err := os.Lstat(target); err != nil {
		target = filepath.Dir(target) // check the existing parent
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	r, ok := RelInside(base, real)
	if !ok {
		return fmt.Errorf("%q resolves outside the project", rel)
	}
	if bad := metadataComponent(r, true); bad != "" {
		return fmt.Errorf("%q resolves into %q", rel, bad)
	}
	return nil
}

// OutTarget splits a path the user typed (--out) into the directory a
// root is opened on and the name written through it. A relative path that
// stays inside cwd is confined to cwd like an entry: every component is
// checked for symlinks and it may not enter .git*/.dbdiagram. An absolute
// path, or one leaving cwd, is the explicit escape hatch: a root on its own
// directory (only the final component is guarded). Never a layout name.
func OutTarget(cwd, out string) (dir, name string, err error) {
	if hasControl(out) {
		return "", "", fmt.Errorf("--out %q must not contain control characters", out)
	}
	abs := out
	if !filepath.IsAbs(out) {
		abs = filepath.Join(cwd, out)
	}
	if strings.HasSuffix(strings.ToLower(filepath.Base(abs)), LAYOUT_EXT) {
		return "", "", fmt.Errorf("--out %q must not be a %s layout file", out, LAYOUT_EXT)
	}
	if !filepath.IsAbs(out) {
		if rel, ok := RelInside(cwd, abs); ok && rel != "." {
			if bad := metadataComponent(rel, runtime.GOOS == "windows"); bad != "" {
				return "", "", fmt.Errorf("--out %q must not be inside %q", out, bad)
			}
			return cwd, rel, nil
		}
	}
	dir = filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	return dir, filepath.Base(abs), nil
}

// WriteOut writes a --out path (see OutTarget).
func WriteOut(cwd, out string, b []byte) error {
	dir, name, err := OutTarget(cwd, out)
	if err != nil {
		return err
	}
	p, err := OpenProject(dir)
	if err != nil {
		return err
	}
	defer p.Close()
	return p.WriteFile(name, b, 0o644)
}

// ReadExplicit reads a file in a directory the CLI chose (credentials),
// with the regular-file and size rules. Missing files wrap fs.ErrNotExist.
func ReadExplicit(path string, max int64) ([]byte, error) {
	p, err := OpenProject(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return p.ReadRegular(filepath.Base(path), max)
}

// RelInside returns target relative to base, or ok=false when target is
// outside base.
func RelInside(base, target string) (string, bool) {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
