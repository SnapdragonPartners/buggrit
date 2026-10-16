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
// are slash-separated and relative to the repository root.
type Workspace = fs.FS

// OpenDir opens dir as a Workspace. Access goes through os.Root, so no path
// or symlink can reach outside dir (design §10). Call close when done.
func OpenDir(dir string) (ws Workspace, closeFn func() error, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("open workspace %s: %w", dir, err)
	}
	return root.FS(), root.Close, nil
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
