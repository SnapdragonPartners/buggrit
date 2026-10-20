// Package scripts holds tests for the shell scripts the composite action
// runs. The scripts aren't Go; the tests drive them against a fake gh.
package scripts

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGH answers the gh calls install.sh makes from files under $FAKE_GH:
// commits/<ref> holds the ref's SHA, tags lists "<name> <sha>" lines, and
// releases/<tag>/ holds the assets.
const fakeGH = `#!/usr/bin/env bash
set -euo pipefail
echo "$*" >> "$FAKE_GH/calls"
if [ "$1" = api ]; then
  shift
  [ "$1" = --paginate ] && shift
  case "$1" in
    */commits/*) cat "$FAKE_GH/commits/${1##*/}" ;;
    */tags*)
      sha=$(sed -n 's/.*select(.commit.sha == "\([0-9a-f]*\)").*/\1/p' <<< "$3")
      awk -v sha="$sha" '$2 == sha { print $1 }' "$FAKE_GH/tags" ;;
  esac
  exit 0
fi
if [ "$1" = release ] && [ "$2" = download ]; then
  tag="$3"; shift 3; dir=""; pats=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --dir) dir="$2"; shift 2 ;;
      --pattern) pats+=("$2"); shift 2 ;;
      *) shift 2 ;;
    esac
  done
  [ -d "$FAKE_GH/releases/$tag" ] || { echo "release not found" >&2; exit 1; }
  for p in "${pats[@]}"; do cp "$FAKE_GH/releases/$tag/$p" "$dir/"; done
  exit 0
fi
echo "fake gh: unexpected call: $*" >&2
exit 1
`

type fixture struct {
	t     *testing.T
	root  string // FAKE_GH
	path  string // PATH with the fake gh first
	os    string // Actions RUNNER_OS
	arch  string // Actions RUNNER_ARCH
	asset string // archive name for this runner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	for _, tool := range []string{"bash", "tar", "install"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	f := &fixture{t: t, root: t.TempDir()}
	switch runtime.GOOS {
	case "linux":
		f.os = "Linux"
	case "darwin":
		f.os = "macOS"
	default:
		t.Skipf("install.sh doesn't support %s", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64":
		f.arch = "X64"
	case "arm64":
		f.arch = "ARM64"
	default:
		t.Skipf("install.sh doesn't support %s", runtime.GOARCH)
	}
	f.asset = fmt.Sprintf("buggrit_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	bin := filepath.Join(f.root, "bin")
	f.write("bin/gh", fakeGH, 0o755)
	f.path = bin + string(os.PathListSeparator) + os.Getenv("PATH")
	f.write("tags", "", 0o644)
	return f
}

func (f *fixture) write(rel, content string, mode os.FileMode) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		f.t.Fatal(err)
	}
}

// release publishes tag at sha, with a fake binary that prints the tag.
func (f *fixture) release(tag, sha string) {
	f.t.Helper()
	f.write("commits/"+tag, sha+"\n", 0o644)
	tags, err := os.ReadFile(filepath.Join(f.root, "tags"))
	if err != nil {
		f.t.Fatal(err)
	}
	f.write("tags", string(tags)+tag+" "+sha+"\n", 0o644)
	archive := tarball(f.t, "#!/bin/sh\necho "+tag+"\n")
	f.write("releases/"+tag+"/"+f.asset, string(archive), 0o644)
	sum := sha256.Sum256(archive)
	f.write("releases/"+tag+"/checksums.txt", hex.EncodeToString(sum[:])+"  "+f.asset+"\n", 0o644)
}

// run runs install.sh and returns its output and the installed binary's
// output ("" if nothing was installed).
func (f *fixture) run(env ...string) (string, string, error) {
	f.t.Helper()
	binDir := filepath.Join(f.t.TempDir(), "out")
	cmd := exec.Command("bash", "install.sh") //nolint:noctx // a test of a local script
	cmd.Env = append([]string{
		"PATH=" + f.path,
		"FAKE_GH=" + f.root,
		"GH_TOKEN=token",
		"BIN_DIR=" + binDir,
		"RUNNER_OS=" + f.os,
		"RUNNER_ARCH=" + f.arch,
	}, env...)
	out, err := cmd.CombinedOutput()
	installed, _ := exec.Command(filepath.Join(binDir, "buggrit")).Output() //nolint:noctx,gosec // the test's own temp dir
	return string(out), strings.TrimSpace(string(installed)), err
}

func tarball(t *testing.T, script string) []byte {
	t.Helper()
	var buf strings.Builder
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, file := range []struct{ name, body string }{{"README.md", "readme\n"}, {"buggrit", script}} {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0o755, Size: int64(len(file.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(buf.String())
}

const shaA, shaB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestInstall(t *testing.T) {
	cases := []struct {
		name    string
		env     []string
		want    string // installed version, or "" for a failure
		wantErr string
	}{
		{name: "release tag", env: []string{"REPO=o/buggrit", "REF=v0.1.0"}, want: "v0.1.0"},
		{name: "moving tag runs the newest release at its commit", env: []string{"REPO=o/buggrit", "REF=v0"}, want: "v0.2.1"},
		{name: "SHA of a release", env: []string{"REPO=o/buggrit", "REF=" + shaA}, want: "v0.1.0"},
		{name: "version overrides the ref", env: []string{"REPO=o/buggrit", "REF=v0", "VERSION=v0.1.0"}, want: "v0.1.0"},
		{
			name: "repo and ref from the action path",
			env:  []string{"GITHUB_ACTION_PATH=/runner/_actions/o/buggrit/v0/"},
			want: "v0.2.1",
		},
		{name: "commit with no release", env: []string{"REPO=o/buggrit", "REF=main"}, wantErr: "no release tag at main"},
		{name: "unknown release", env: []string{"REPO=o/buggrit", "VERSION=v9.9.9"}, wantErr: "release not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.release("v0.1.0", shaA)
			f.release("v0.2.0", shaB)
			f.release("v0.2.1", shaB) // a re-tag of the same commit; the newest wins
			f.write("commits/v0", shaB+"\n", 0o644)
			f.write("tags", mustRead(t, filepath.Join(f.root, "tags"))+"v0 "+shaB+"\n", 0o644)
			f.write("commits/"+shaA, shaA+"\n", 0o644)
			f.write("commits/main", "cccccccccccccccccccccccccccccccccccccccc\n", 0o644)

			out, got, err := f.run(tc.env...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(out, tc.wantErr) {
					t.Fatalf("err = %v, output:\n%s\nwant it to contain %q", err, out, tc.wantErr)
				}
				if got != "" {
					t.Errorf("installed %q after a failure", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			if got != tc.want {
				t.Errorf("installed %q, want %q\n%s", got, tc.want, out)
			}
		})
	}
}

func TestInstallRejectsBadChecksum(t *testing.T) {
	f := newFixture(t)
	f.release("v0.1.0", shaA)
	f.write("releases/v0.1.0/checksums.txt", strings.Repeat("0", 64)+"  "+f.asset+"\n", 0o644)
	out, got, err := f.run("REPO=o/buggrit", "REF=v0.1.0")
	if err == nil {
		t.Fatalf("install succeeded with a bad checksum:\n%s", out)
	}
	if got != "" {
		t.Errorf("installed %q despite a bad checksum", got)
	}
}

func TestInstallRejectsMissingChecksum(t *testing.T) {
	f := newFixture(t)
	f.release("v0.1.0", shaA)
	f.write("releases/v0.1.0/checksums.txt", strings.Repeat("0", 64)+"  buggrit_other_os.tar.gz\n", 0o644)
	out, _, err := f.run("REPO=o/buggrit", "REF=v0.1.0")
	if err == nil || !strings.Contains(out, "is not in the checksums") {
		t.Fatalf("err = %v, output:\n%s", err, out)
	}
}

func TestInstallRejectsUnsupportedRunner(t *testing.T) {
	f := newFixture(t)
	f.os = "Windows"
	out, _, err := f.run("REPO=o/buggrit", "REF=v0.1.0")
	if err == nil || !strings.Contains(out, "no release binary for Windows") {
		t.Fatalf("err = %v, output:\n%s", err, out)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
