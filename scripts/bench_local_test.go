package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGo builds by writing a fake binary: buggrit-bench, or buggrit with
// BUILT set to the commit its -C directory is at, as the real build info
// would carry it.
const fakeGo = `#!/usr/bin/env bash
[ "$1" = build ] || { echo "fake go: unexpected call: $*" >&2; exit 1; }
shift
dir=. out=
while [ $# -gt 1 ]; do
  case $1 in -C) dir=$2; shift ;; -o) out=$2; shift ;; esac
  shift
done
case $1 in
  ./cmd/buggrit-bench) cp "$FAKE_BIN/bench" "$out" ;;
  ./cmd/buggrit) { echo '#!/usr/bin/env bash'; echo "BUILT=$(git -C "$dir" rev-parse HEAD)"; cat "$FAKE_BIN/buggrit"; } >"$out" ;;
  *) echo "fake go: unexpected package $1" >&2; exit 1 ;;
esac
chmod +x "$out"
`

// fakeBench logs each call. prepare writes a two-run matrix of arm "a" at
// FAKE_COMMIT, reviewing o/target#7 at FAKE_HEAD with a config that names
// ARM_KEY; report exits FAKE_REPORT_EXIT.
const fakeBench = `#!/usr/bin/env bash
echo "bench $*" >>"$FAKE_LOG"
cmd=$1
shift
out=
while [ $# -gt 0 ]; do
  case $1 in --output | --out) out=$2; shift ;; esac
  shift
done
case $cmd in
  prepare)
    cfg='{"models":{"default":{"api_key_env":"ARM_KEY"}}}'
    matrix=$(jq -cn --arg c "$FAKE_COMMIT" --arg h "$FAKE_HEAD" --arg cfg "$cfg" \
      '{include: [range(1; 3) as $n | {arm: "a", ref: "main", commit: $c, run: $n, label: "exp/a", config: $cfg, repo: "o/target", pr: 7, head: $h, timeout: 30}]}')
    printf 'commits={"main":"%s"}\nmatrix=%s\nparallel=2\nreplay={"repo":"o/target","pr":7}\n' "$FAKE_COMMIT" "$matrix" >>"$out" ;;
  report)
    echo "report env: repository=${GITHUB_REPOSITORY-unset}" >>"$FAKE_LOG"
    echo report >"$out"
    exit "${FAKE_REPORT_EXIT:-0}" ;;
esac
`

// fakeBuggrit answers db status with FAKE_DB_STATUS, and logs each review:
// the commit it was built from, the environment that files its telemetry,
// the workspace's head, whether an earlier run's stray file is still there,
// and the config. A review leaves a stray file, interrupts the script when
// FAKE_INTERRUPT is set, and exits FAKE_REVIEW_EXIT.
const fakeBuggrit = `case $1 in
  db) echo "${FAKE_DB_STATUS:-buggrit schema: v4, up to date}" ;;
  review)
    ws= cfg=
    while [ $# -gt 0 ]; do
      case $1 in --workspace) ws=$2; shift ;; --config) cfg=$2; shift ;; esac
      shift
    done
    stray=no
    [ ! -e "$ws/stray" ] || stray=yes
    echo "review built=$BUILT run=$GITHUB_RUN_ID attempt=$GITHUB_RUN_ATTEMPT label=$BUGGRIT_TELEMETRY_LABEL repository=${GITHUB_REPOSITORY-unset} head=$(git -C "$ws" rev-parse HEAD) stray=$stray config=$(cat "$cfg")" >>"$FAKE_LOG"
    touch "$ws/stray"
    [ -z "${FAKE_INTERRUPT:-}" ] || kill -INT "$PPID"
    exit "${FAKE_REVIEW_EXIT:-0}" ;;
esac
`

// fakeBenchGH clones o/target from FAKE_TARGET_ORIGIN.
const fakeBenchGH = `#!/usr/bin/env bash
[ "$1 $2 $3" = "repo clone o/target" ] && [ "$5" = -- ] || { echo "fake gh: unexpected call: $*" >&2; exit 1; }
dir=$4
shift 5
git clone "$@" "$FAKE_TARGET_ORIGIN" "$dir"
`

// benchLocal is a buggrit checkout holding bench-local.sh, whose origin
// has the arm's commit, and a target repository.
type benchLocal struct {
	*repo
	root, log, armCommit, targetHead string
}

func newBenchLocal(t *testing.T) *benchLocal {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not found")
	}
	r := newRepo(t)
	root := filepath.Dir(r.dir)
	script, err := os.ReadFile("bench-local.sh")
	if err != nil {
		t.Fatal(err)
	}
	r.write("scripts/bench-local.sh", string(script))
	arm := r.commit("bench-local")
	r.git(r.dir, "push", "-q", "origin", "main")

	target := filepath.Join(root, "target-work")
	r.git(root, "init", "-q", "-b", "main", target)
	if err := os.WriteFile(filepath.Join(target, "x.go"), []byte("package x\n"), 0o644); err != nil { //nolint:gosec // test files
		t.Fatal(err)
	}
	r.git(target, "add", "-A")
	r.git(target, "commit", "-q", "-m", "target")
	head := r.git(target, "rev-parse", "HEAD")
	r.git(root, "clone", "-q", "--bare", target, filepath.Join(root, "target.git"))

	fakes := filepath.Join(root, "fakes")
	bin := filepath.Join(root, "bin")
	for dir, files := range map[string]map[string]string{
		fakes: {"bench": fakeBench, "buggrit": fakeBuggrit},
		bin:   {"go": fakeGo, "gh": fakeBenchGH},
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil { //nolint:gosec // executable fakes
				t.Fatal(err)
			}
		}
	}
	b := &benchLocal{repo: r, root: root, log: filepath.Join(root, "log"), armCommit: arm, targetHead: head}
	b.env = append(b.env,
		"FAKE_BIN="+fakes, "FAKE_LOG="+b.log, "FAKE_COMMIT="+arm, "FAKE_HEAD="+head,
		"FAKE_TARGET_ORIGIN="+filepath.Join(root, "target.git"),
		"BENCH_REPO=o/buggrit", "BENCH_RUN_ID=4242", "BENCH_DIR="+filepath.Join(root, "bench"),
		"GITHUB_TOKEN=t", "BUGGRIT_DATABASE_URL=postgres://db",
		// Left over from a shell; the script must keep them from the runs.
		"GITHUB_REPOSITORY=stray/repo", "BUGGRIT_TELEMETRY_LABEL=stray/label",
	)
	if err := os.WriteFile(filepath.Join(root, "exp.json"), []byte(`{"judge": {"api_key_env": "JUDGE_KEY"}}`), 0o644); err != nil { //nolint:gosec // test files
		t.Fatal(err)
	}
	return b
}

// run runs the repository's copy of bench-local.sh and returns its exit
// code and the fakes' log.
func (b *benchLocal) run(env ...string) (int, []string) {
	b.t.Helper()
	cmd := exec.Command("bash", filepath.Join(b.dir, "scripts", "bench-local.sh"), filepath.Join(b.root, "exp.json")) //nolint:noctx,gosec // a test of local scripts
	cmd.Dir = b.dir
	cmd.Env = append(append([]string{}, b.env...), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		b.t.Fatal(err)
	}
	b.t.Logf("bench-local.sh exited %d:\n%s", code, out)
	data, err := os.ReadFile(b.log)
	if err != nil && !os.IsNotExist(err) {
		b.t.Fatal(err)
	}
	return code, strings.Split(strings.TrimSpace(string(data)), "\n")
}

// calls is each log line's first word and, for bench calls, its command.
func calls(log []string) []string {
	var out []string
	for _, l := range log {
		f := strings.Fields(l)
		switch {
		case len(f) == 0:
		case f[0] == "bench" && len(f) > 1:
			out = append(out, "bench "+f[1])
		case f[0] == "report":
			// the report's environment line
		default:
			out = append(out, f[0])
		}
	}
	return out
}

func TestBenchLocal(t *testing.T) {
	keys := []string{"JUDGE_KEY=j", "ARM_KEY=a"}
	full := []string{"bench check", "bench prepare", "review", "review", "bench report", "bench cleanup"}
	for _, tc := range []struct {
		name  string
		env   []string
		code  int
		calls []string
	}{
		{name: "complete", env: keys, calls: full},
		{name: "an incomplete pass is a result", env: append([]string{"FAKE_REVIEW_EXIT=1"}, keys...), calls: full},
		{name: "a judge key that isn't set stops before prepare", env: []string{"ARM_KEY=a"}, code: 1,
			calls: []string{"bench check"}},
		{name: "an arm key that isn't set stops before any review", env: []string{"JUDGE_KEY=j"}, code: 1,
			calls: []string{"bench check", "bench prepare", "bench cleanup"}},
		{name: "a database behind the arm stops before any review", code: 1,
			env:   append([]string{"FAKE_DB_STATUS=buggrit schema: v1; this binary needs v2"}, keys...),
			calls: []string{"bench check", "bench prepare", "bench cleanup"}},
		{name: "a failed report still cleans up", env: append([]string{"FAKE_REPORT_EXIT=1"}, keys...), code: 1, calls: full},
		{name: "an interrupt reports what ran and cleans up", env: append([]string{"FAKE_INTERRUPT=1"}, keys...), code: 130,
			calls: []string{"bench check", "bench prepare", "review", "bench report", "bench cleanup"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBenchLocal(t)
			code, log := b.run(tc.env...)
			if code != tc.code {
				t.Errorf("exit code %d, want %d", code, tc.code)
			}
			if got := strings.Join(calls(log), ", "); got != strings.Join(tc.calls, ", ") {
				t.Errorf("calls: %s\nwant:  %s", got, strings.Join(tc.calls, ", "))
			}
			if wt := b.git(b.dir, "worktree", "list"); strings.Count(wt, "\n") != 0 {
				t.Errorf("arm worktrees left behind:\n%s", wt)
			}
			b.checkLog(log)
		})
	}
}

// checkLog checks the arguments and environment of each call made.
func (b *benchLocal) checkLog(log []string) {
	b.t.Helper()
	replay := `--replay {"repo":"o/target","pr":7}`
	for _, l := range log {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		var want []string
		switch {
		case f[0] == "bench" && f[1] == "prepare":
			want = []string{"--workflow-run 4242 --attempt 1 --buggrit-repo o/buggrit --output "}
		case f[0] == "review":
			// Each review leaves a stray file; the checkout is cleaned
			// between runs.
			want = []string{
				"built=" + b.armCommit, "run=4242 attempt=1 label=exp/a repository=unset",
				"head=" + b.targetHead, "stray=no", `config={"models":{"default":{"api_key_env":"ARM_KEY"}}}`,
			}
		case f[0] == "bench" && f[1] == "report":
			want = []string{replay, `--commits {"main":"` + b.armCommit + `"}`, "--workflow-run 4242 --attempt 1"}
		case f[0] == "report":
			want = []string{"repository=unset"}
		case f[0] == "bench" && f[1] == "cleanup":
			want = []string{replay}
		}
		for _, w := range want {
			if !strings.Contains(l, w) {
				b.t.Errorf("%s\n  missing %q", l, w)
			}
		}
	}
}
