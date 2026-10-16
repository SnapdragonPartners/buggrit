// Package review is buggrit's review engine: the agentic finder today, and
// later the verifier and reconcile stages (docs/design.md §2, §5).
//
// The engine sees the checkout only through a Workspace, a read-only fs.FS,
// and reaches models only through llms.ChatClient, so it carries nothing
// specific to GitHub Actions.
package review

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

// Workspace is a read-only view of the checkout at the reviewed head. Paths
// are slash-separated and relative to the repository root. It must report
// symlinks (fs.ReadLinkFS) so the tools can refuse to follow them.
type Workspace = fs.ReadLinkFS

// OpenDir opens dir as a Workspace. Access goes through os.Root, so no path
// or symlink can reach outside dir (design §10). Call close when done.
func OpenDir(dir string) (ws Workspace, closeFn func() error, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("open workspace %s: %w", dir, err)
	}
	rfs, ok := root.FS().(fs.ReadLinkFS)
	if !ok {
		_ = root.Close()
		return nil, nil, errors.New("open workspace: os.Root's filesystem does not report symlinks")
	}
	return rfs, root.Close, nil
}

// checkNoSymlink refuses a path that goes through a symlink. os.Root keeps
// links inside the checkout, but a link inside it can still point into .git.
// Refusing every link, and naming its target, lets the model ask for the
// target directly, where cleanPath applies the usual rules.
func checkNoSymlink(ws Workspace, name string) error {
	if name == "." {
		return nil
	}
	prefix := ""
	for seg := range strings.SplitSeq(name, "/") {
		prefix = path.Join(prefix, seg)
		info, err := ws.Lstat(prefix)
		if err != nil {
			return nil //nolint:nilerr // the read that follows reports a missing path
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		target, err := ws.ReadLink(prefix)
		if err != nil {
			return fmt.Errorf("%s is a symbolic link; it is not followed", prefix)
		}
		resolved := path.Join(path.Dir(prefix), target)
		if path.IsAbs(target) || !fs.ValidPath(resolved) {
			return fmt.Errorf("%s is a symbolic link pointing outside the repository", prefix)
		}
		return fmt.Errorf("%s is a symbolic link to %s; symlinks are not followed, so use that path instead", prefix, resolved)
	}
	return nil
}

// readFile reads a clean path, refusing symlinks.
func readFile(ws Workspace, name string) ([]byte, error) {
	if err := checkNoSymlink(ws, name); err != nil {
		return nil, err
	}
	return fs.ReadFile(ws, name) //nolint:wrapcheck // callers describe the error for the model
}

// readDir lists a clean path, refusing symlinks.
func readDir(ws Workspace, name string) ([]fs.DirEntry, error) {
	if err := checkNoSymlink(ws, name); err != nil {
		return nil, err
	}
	return fs.ReadDir(ws, name) //nolint:wrapcheck // callers describe the error for the model
}

// errHidden is returned for paths the tools never expose.
var errHidden = errors.New("path is not available to review tools")

// cleanPath turns a model-supplied path into a valid fs.FS path, or explains
// why it can't be one. ".git" is refused at any depth: it can hold
// credentials and review never needs it (diffs come from get_diff).
func cleanPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "/" {
		return ".", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%q: use a path relative to the repository root", p)
	}
	p = path.Clean(p)
	if !fs.ValidPath(p) {
		return "", fmt.Errorf("%q: path must stay inside the repository", p)
	}
	if hiddenPath(p) {
		return "", fmt.Errorf("%q: %w", p, errHidden)
	}
	return p, nil
}

// gitDir is the directory the tools never expose.
const gitDir = ".git"

// hiddenPath reports whether a clean path is inside .git.
func hiddenPath(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == gitDir {
			return true
		}
	}
	return false
}

// isBinary reports whether data looks binary: a NUL byte in the first 8 KiB.
func isBinary(data []byte) bool {
	n := min(len(data), 8<<10)
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}
