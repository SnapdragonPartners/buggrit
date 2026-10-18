// Package review is buggrit's review engine: the agentic verifier and
// finder, and reconcile (docs/design.md §2, §5, §6).
//
// The engine sees the checkout only through a Workspace, a read-only fs.FS,
// and reaches models only through llms.ChatClient, so it carries nothing
// specific to GitHub Actions.
package review

import (
	"bufio"
	"errors"
	"fmt"
	"io"
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

// errBinary reports a file that looks binary (see isBinary).
var errBinary = errors.New("binary file")

// errIsDir reports a directory where a file was expected.
var errIsDir = errors.New("is a directory")

// scanLines streams a clean path's lines to fn, refusing symlinks, binary
// files and directories. Memory stays bounded whatever the file's size: fn
// gets each line without its terminator, clipped to just past
// maxReadLineBytes, and the line's full length. The slice is reused, so fn
// must copy what it keeps. fn returns false to stop early; otherwise the scan
// runs to the end, and total is the file's line count. A final newline
// doesn't start an extra empty line.
func scanLines(ws Workspace, name string, fn func(n int, line []byte, size int) bool) (total int, err error) {
	f, r, err := openText(ws, name)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return eachLine(r, fn)
}

// openText opens a clean path for line scanning, refusing symlinks,
// directories and binary files.
func openText(ws Workspace, name string) (fs.File, *bufio.Reader, error) {
	if err := checkNoSymlink(ws, name); err != nil {
		return nil, nil, err
	}
	f, err := ws.Open(name)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // callers describe the error for the model
	}
	r := bufio.NewReaderSize(f, 64<<10)
	var cause error
	if info, serr := f.Stat(); serr == nil && info.IsDir() {
		cause = errIsDir
	} else if head, _ := r.Peek(8 << 10); isBinary(head) {
		cause = errBinary
	}
	if cause != nil {
		_ = f.Close()
		return nil, nil, cause
	}
	return f, r, nil
}

// eachLine is scanLines' loop over an open reader.
func eachLine(r *bufio.Reader, fn func(n int, line []byte, size int) bool) (total int, err error) {
	var line []byte // the current line, clipped
	size := 0       // the current line's full length
	for {
		chunk, rerr := r.ReadSlice('\n')
		full := errors.Is(rerr, bufio.ErrBufferFull)
		if rerr != nil && !full && !errors.Is(rerr, io.EOF) {
			return total, rerr //nolint:wrapcheck // callers describe the error for the model
		}
		ended := rerr == nil // chunk ends with the line's newline
		if ended {
			chunk = chunk[:len(chunk)-1]
		}
		size += len(chunk)
		if room := maxReadLineBytes + 1 - len(line); room > 0 {
			line = append(line, chunk[:min(room, len(chunk))]...)
		}
		switch {
		case full:
			continue // more of a long line
		case !ended && size == 0:
			return total, nil // EOF with no unterminated last line
		}
		total++
		if fn != nil && !fn(total, line, size) || !ended {
			return total, nil
		}
		line, size = line[:0], 0
	}
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
