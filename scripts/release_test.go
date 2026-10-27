package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// reviewYML is the part of review.yml the release scripts care about: the
// snippet in its comment and buggrit's own uses: lines.
const reviewYML = `#       uses: SnapdragonPartners/buggrit/.github/workflows/review.yml@v0
jobs:
  review:
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - name: Connect to the database
        uses: SnapdragonPartners/buggrit/.github/actions/database@v0
      - name: Review
        uses: SnapdragonPartners/buggrit@v0
`

// fakeReleaseGH answers the gh calls release.sh and move-majors.sh make,
// already filtered as their --jq would: FAKE_BLOCKERS lists open
// release-blocker issues, FAKE_IMMUTABLE is the immutable-releases setting,
// FAKE_RULESETS names the active tag rulesets, FAKE_ADMIN is whether the
// caller is an admin, FAKE_PUBLISHED=no makes every release still building
// (a 404), and FAKE_FINALS lists the published releases (default: v0.5.0).
const fakeReleaseGH = `#!/usr/bin/env bash
case "$1 $2" in
  "issue list") printf '%s' "${FAKE_BLOCKERS:-}" ;;
  "api repos/o/buggrit/releases/tags/"*)
    [ "${FAKE_PUBLISHED:-yes}" = yes ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
    printf 'buggrit_linux_amd64.tar.gz\nchecksums.txt\n' ;;
  "release list") printf '%b\n' "${FAKE_FINALS-v0.5.0}" ;;
  "api repos/o/buggrit/immutable-releases") echo "${FAKE_IMMUTABLE:-true}" ;;
  "api repos/o/buggrit/rulesets") printf '%s\n' "${FAKE_RULESETS-Release tags}" ;;
  "api repos/o/buggrit") echo "${FAKE_ADMIN:-true}" ;;
  *) echo "fake gh: unexpected call: $*" >&2; exit 1 ;;
esac
`

// repo is a clone of a bare origin whose main holds reviewYML.
type repo struct {
	t      *testing.T
	dir    string
	origin string
	env    []string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	for _, tool := range []string{"bash", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	root := t.TempDir()
	gitconfig := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(gitconfig, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeReleaseGH), 0o755); err != nil { //nolint:gosec // an executable fake
		t.Fatal(err)
	}
	r := &repo{t: t, dir: filepath.Join(root, "work"), origin: filepath.Join(root, "origin.git")}
	r.env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL="+gitconfig, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"REPO=o/buggrit",
	)
	r.git(root, "init", "-q", "--bare", "-b", "main", r.origin)
	r.git(root, "init", "-q", "-b", "main", r.dir)
	r.git(r.dir, "remote", "add", "origin", r.origin)
	r.write(".github/workflows/review.yml", reviewYML)
	r.write("README.md", "buggrit\n")
	r.commit("main")
	r.git(r.dir, "push", "-q", "origin", "main")
	r.git(r.dir, "fetch", "-q", "origin")
	return r
}

func (r *repo) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...) //nolint:noctx // a test of local scripts
	cmd.Dir = dir
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil { //nolint:gosec // test files
		r.t.Fatal(err)
	}
}

func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.git(r.dir, "add", "-A")
	r.git(r.dir, "commit", "-q", "-m", msg)
	return r.git(r.dir, "rev-parse", "HEAD")
}

// run runs one of the scripts in this directory from the repo.
func (r *repo) run(script string, env []string, args ...string) (string, error) {
	r.t.Helper()
	path, err := filepath.Abs(script)
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{path}, args...)...) //nolint:noctx,gosec // a test of local scripts
	cmd.Dir = r.dir
	cmd.Env = append(append([]string{}, r.env...), env...)
	detach(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *repo) read(rel string) string {
	r.t.Helper()
	return mustRead(r.t, filepath.Join(r.dir, rel))
}

func TestPinRelease(t *testing.T) {
	const pinned = `#       uses: SnapdragonPartners/buggrit/.github/workflows/review.yml@v0
jobs:
  review:
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - name: Connect to the database
        uses: SnapdragonPartners/buggrit/.github/actions/database@v0.5.0-rc.1
      - name: Review
        uses: SnapdragonPartners/buggrit@v0.5.0-rc.1
`
	t.Run("pins buggrit's own uses lines and nothing else", func(t *testing.T) {
		r := newRepo(t)
		if out, err := r.run("pin-release.sh", nil, "v0.5.0-rc.1", r.dir); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got := r.read(".github/workflows/review.yml"); got != pinned {
			t.Errorf("review.yml:\n%s\nwant:\n%s", got, pinned)
		}
	})

	cases := []struct {
		name    string
		yml     string
		version string
		wantErr string
	}{
		{name: "nothing to pin", yml: pinned, version: "v0.5.0", wantErr: "no @v0 references"},
		{name: "not a version", yml: reviewYML, version: "v0", wantErr: "is not a vX.Y.Z version"},
		{
			name:    "another ref left behind",
			yml:     reviewYML + "      - uses: SnapdragonPartners/buggrit/.github/actions/other@main\n",
			version: "v0.5.0",
			wantErr: "not pinned to v0.5.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			r.write(".github/workflows/review.yml", tc.yml)
			out, err := r.run("pin-release.sh", nil, tc.version, r.dir)
			if err == nil || !strings.Contains(out, tc.wantErr) {
				t.Errorf("err = %v, output %q; want an error with %q", err, out, tc.wantErr)
			}
		})
	}
}

func TestVerifyRelease(t *testing.T) {
	// release commits a pinned review.yml on top of base, plus edit, and
	// tags it.
	release := func(r *repo, base, tag, pin string, edit func()) {
		r.git(r.dir, "checkout", "-q", "--detach", base)
		if out, err := r.run("pin-release.sh", nil, pin, r.dir); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if edit != nil {
			edit()
		}
		r.git(r.dir, "tag", tag, r.commit("Release "+tag))
		r.git(r.dir, "checkout", "-q", "main")
	}

	cases := []struct {
		name    string
		setup   func(r *repo)
		tag     string // default v0.5.0
		wantErr string // "" for a pass
	}{
		{
			name:  "main plus the pin",
			setup: func(r *repo) { release(r, "main", "v0.5.0", "v0.5.0", nil) },
		},
		{
			name: "an older commit on main",
			setup: func(r *repo) {
				old := r.git(r.dir, "rev-parse", "HEAD")
				r.write("README.md", "newer\n")
				r.commit("newer")
				r.git(r.dir, "push", "-q", "origin", "main")
				r.git(r.dir, "fetch", "-q", "origin")
				release(r, old, "v0.4.1", "v0.4.1", nil)
			},
			tag: "v0.4.1",
		},
		{
			name: "an extra change",
			setup: func(r *repo) {
				release(r, "main", "v0.5.0", "v0.5.0", func() { r.write("README.md", "sneaky\n") })
			},
			wantErr: "isn't exactly its parent plus",
		},
		{
			name:    "pinned to another version",
			setup:   func(r *repo) { release(r, "main", "v0.5.0", "v0.5.1", nil) },
			wantErr: "isn't exactly its parent plus",
		},
		{
			name: "main's own commit",
			setup: func(r *repo) {
				r.write("README.md", "newer\n")
				r.commit("newer")
				r.git(r.dir, "push", "-q", "origin", "main")
				r.git(r.dir, "fetch", "-q", "origin")
				r.git(r.dir, "tag", "v0.5.0", "main")
			},
			wantErr: "isn't exactly its parent plus",
		},
		{
			name: "parent not on main",
			setup: func(r *repo) {
				r.git(r.dir, "checkout", "-q", "-b", "side")
				r.write("README.md", "side\n")
				side := r.commit("side")
				r.git(r.dir, "checkout", "-q", "main")
				release(r, side, "v0.5.0", "v0.5.0", nil)
			},
			wantErr: "isn't on origin/main",
		},
		{
			name:    "a root commit",
			setup:   func(r *repo) { r.git(r.dir, "tag", "v0.5.0", "main") },
			wantErr: "exactly one parent",
		},
		{
			name: "a merge",
			setup: func(r *repo) {
				r.git(r.dir, "checkout", "-q", "-b", "side")
				r.write("other.txt", "side\n")
				r.commit("side")
				r.git(r.dir, "checkout", "-q", "main")
				r.git(r.dir, "merge", "-q", "--no-ff", "-m", "merge", "side")
				r.git(r.dir, "tag", "v0.5.0", "HEAD")
			},
			wantErr: "exactly one parent",
		},
		{
			name:    "no such tag",
			setup:   func(*repo) {},
			wantErr: "no such tag",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			tc.setup(r)
			tag := tc.tag
			if tag == "" {
				tag = "v0.5.0"
			}
			out, err := r.run("verify-release.sh", nil, tag)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v\n%s", err, out)
			case tc.wantErr != "" && (err == nil || !strings.Contains(out, tc.wantErr)):
				t.Errorf("err = %v, output %q; want an error with %q", err, out, tc.wantErr)
			}
			if wt := r.git(r.dir, "worktree", "list"); strings.Count(wt, "\n") != 0 {
				t.Errorf("worktrees left behind:\n%s", wt)
			}
		})
	}
}

func TestRelease(t *testing.T) {
	yes := []string{"RELEASE_CONFIRM=yes", "RELEASE_POLL=0"}

	t.Run("pushes a verified release tag, then moves v0 to it", func(t *testing.T) {
		r := newRepo(t)
		main := r.git(r.dir, "rev-parse", "HEAD")
		out, err := r.run("release.sh", yes, "v0.5.0")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		pushed := r.git(r.origin, "rev-parse", "refs/tags/v0.5.0^{commit}")
		if v0 := r.git(r.origin, "rev-parse", "refs/tags/v0"); v0 != pushed {
			t.Errorf("origin's v0 = %s, want the release commit %s", v0, pushed)
		}
		if parent := r.git(r.origin, "rev-parse", pushed+"^"); parent != main {
			t.Errorf("release commit's parent = %s, want main %s", parent, main)
		}
		yml := r.git(r.origin, "show", pushed+":.github/workflows/review.yml")
		if !strings.Contains(yml+"\n", "uses: SnapdragonPartners/buggrit@v0.5.0\n") {
			t.Errorf("pushed review.yml isn't pinned:\n%s", yml)
		}
		if branch := r.git(r.dir, "rev-parse", "--abbrev-ref", "HEAD"); branch != "main" {
			t.Errorf("left on %s, want main", branch)
		}
		if status := r.git(r.dir, "status", "--porcelain"); status != "" {
			t.Errorf("worktree not clean:\n%s", status)
		}
		if wt := r.git(r.dir, "worktree", "list"); strings.Count(wt, "\n") != 0 {
			t.Errorf("worktrees left behind:\n%s", wt)
		}
		if out, err := r.run("verify-release.sh", nil, "v0.5.0"); err != nil {
			t.Errorf("the pushed tag fails verify-release.sh: %v\n%s", err, out)
		}
	})

	cases := []struct {
		name    string
		env     []string
		ask     bool // prompt instead of RELEASE_CONFIRM=yes
		setup   func(r *repo)
		version string
		wantErr string
		pushed  bool // the release tag reached origin before the failure
	}{
		{name: "not a version", version: "0.5.0", wantErr: "is not a vX.Y.Z version"},
		{name: "dirty worktree", setup: func(r *repo) { r.write("README.md", "edit\n") }, wantErr: "worktree isn't clean"},
		{
			name: "behind origin",
			setup: func(r *repo) {
				r.write("README.md", "newer\n")
				r.commit("newer")
				r.git(r.dir, "push", "-q", "origin", "main")
				r.git(r.dir, "reset", "-q", "--hard", "HEAD^")
			},
			wantErr: "HEAD isn't origin/main",
		},
		{
			name: "tag already on origin",
			setup: func(r *repo) {
				r.git(r.dir, "tag", "v0.5.0")
				r.git(r.dir, "push", "-q", "origin", "v0.5.0")
				r.git(r.dir, "tag", "-d", "v0.5.0")
			},
			wantErr: "already exists on origin",
			pushed:  true,
		},
		{name: "release blockers", env: []string{"FAKE_BLOCKERS=  #43 fence 404"}, wantErr: "open release-blocker issues:\n  #43 fence 404"},
		{name: "mutable releases", env: []string{"FAKE_IMMUTABLE=false"}, wantErr: "immutable releases are off"},
		{name: "no tag ruleset", env: []string{"FAKE_RULESETS="}, wantErr: `no active "Release tags" ruleset`},
		{name: "not an admin", env: []string{"FAKE_ADMIN=false"}, wantErr: "only an admin of o/buggrit"},
		{name: "no terminal to ask", ask: true, wantErr: "not pushed"},
		{
			name: "push rejected",
			setup: func(r *repo) {
				hook := filepath.Join(r.origin, "hooks", "pre-receive")
				if err := os.WriteFile(hook, []byte("#!/bin/sh\necho rejected >&2\nexit 1\n"), 0o755); err != nil { //nolint:gosec // an executable hook
					t.Fatal(err)
				}
			},
			wantErr: "pushing v0.5.0 failed",
		},
		{
			name:    "release not published in time",
			env:     []string{"FAKE_PUBLISHED=no", "RELEASE_WAIT=0"},
			wantErr: "isn't published after 0s; once release.yml has published it, run make move-majors",
			pushed:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			if tc.setup != nil {
				tc.setup(r)
			}
			version := tc.version
			if version == "" {
				version = "v0.5.0"
			}
			env := tc.env
			if !tc.ask {
				env = append(append([]string{}, yes...), env...)
			}
			out, err := r.run("release.sh", env, version)
			if err == nil || !strings.Contains(out, tc.wantErr) {
				t.Errorf("err = %v, output %q; want an error with %q", err, out, tc.wantErr)
			}
			if tags := r.git(r.origin, "tag", "--list"); tc.pushed != (tags == "v0.5.0") {
				t.Errorf("tags on origin: %q, want v0.5.0 only if pushed (%v)", tags, tc.pushed)
			}
			if tags := r.git(r.dir, "tag", "--list"); tags != "" && !tc.pushed {
				t.Errorf("local tags left: %s", tags)
			}
		})
	}
}

func TestMoveMajors(t *testing.T) {
	// setup tags a commit per release on origin and points v0 at the first.
	setup := func(r *repo, releases ...string) map[string]string {
		shas := map[string]string{}
		for _, tag := range releases {
			r.write("README.md", tag+"\n")
			shas[tag] = r.commit(tag)
			r.git(r.dir, "tag", tag)
		}
		r.git(r.dir, "tag", "v0", shas[releases[0]])
		r.git(r.dir, "push", "-q", "origin", "--tags")
		return shas
	}

	t.Run("moves each major to its newest final release", func(t *testing.T) {
		r := newRepo(t)
		shas := setup(r, "v0.4.0", "v0.5.0", "v0.5.1", "v0.6.0-rc.1", "v1.0.0")
		// v0.5.1 is a patch published after v0.5.0, listed out of order;
		// the pre-release never counts.
		out, err := r.run("move-majors.sh", []string{`FAKE_FINALS=v1.0.0\nv0.5.1\nv0.4.0\nv0.5.0\nv0.6.0-rc.1`})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for major, want := range map[string]string{"v0": shas["v0.5.1"], "v1": shas["v1.0.0"]} {
			if got := r.git(r.origin, "rev-parse", "refs/tags/"+major); got != want {
				t.Errorf("%s = %s, want %s", major, got, want)
			}
		}
		out, err = r.run("move-majors.sh", []string{`FAKE_FINALS=v1.0.0\nv0.5.1`})
		if err != nil || !strings.Contains(out, "v0 already points at v0.5.1") {
			t.Errorf("second run: err = %v\n%s", err, out)
		}
	})

	t.Run("no published releases", func(t *testing.T) {
		r := newRepo(t)
		setup(r, "v0.4.0")
		out, err := r.run("move-majors.sh", []string{"FAKE_FINALS="})
		if err == nil || !strings.Contains(out, "no published final releases") {
			t.Errorf("err = %v, output %q", err, out)
		}
	})
}
