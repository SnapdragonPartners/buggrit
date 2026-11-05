#!/usr/bin/env bash
# Runs a benchmark experiment on this machine instead of GitHub Actions
# (docs/benchmarking.md, "Running one locally"), for an arm whose model
# GitHub's runners can't reach, such as one in a local Ollama. It does by
# hand what .github/workflows/bench.yml does, against the same database, so
# the runs, scores and report are recorded exactly as a dispatched
# experiment's are:
#
#   scripts/bench-local.sh my-experiment.json
#
# 1. check the experiment, then prepare: resolve the arms' refs, check the
#    database and open the replay PR, under a made-up workflow run ID;
# 2. build each arm's commit and check the database is migrated for it;
# 3. run every arm × run one at a time (a local model serves one request
#    at a time anyway, so the experiment's `parallel` is ignored), each a
#    `buggrit review --dry-run` of the replay from a checkout of the
#    target's head;
# 4. report: judge the runs, record the scores, write the report;
# 5. cleanup: close the replay, whatever happened after prepare.
#
# Ctrl-C ends the review in progress (it records a canceled pass), skips
# the rest, and still reports and cleans up; Ctrl-C again stops the report
# too, and the replay is still closed.
#
# The run ID defaults to the time in Unix seconds. Actions' run IDs were
# past 4×10^10 in 2026 and only grow, so it can't name a real workflow run,
# and two experiments started a second apart get different IDs. The report
# and the commands to regenerate it name it.
#
# Environment:
#   GITHUB_TOKEN          opens and closes the replay and reads the target
#                         (required)
#   BUGGRIT_DATABASE_URL  the database, through a running Cloud SQL Auth
#                         Proxy (required; docs/benchmarking.md)
#   BENCH_REPO            the repository the arms' refs are in (default
#                         SnapdragonPartners/buggrit)
#   BENCH_REMOTE          git remote for BENCH_REPO here (default origin)
#   BENCH_RUN_ID          the workflow run ID to record under (default: now)
#   BENCH_DIR             where builds, checkouts, logs and the report go:
#                         a new or empty directory (default: a new
#                         temporary one)
#   and each api_key_env the arms' configs and the judge name.
set -euo pipefail

[ $# -eq 1 ] || { echo "usage: bench-local.sh <experiment.json>" >&2; exit 2; }
experiment=$1
[ -f "$experiment" ] || { echo "bench-local: no experiment file $experiment" >&2; exit 2; }
experiment=$(cd "$(dirname "$experiment")" && pwd)/$(basename "$experiment")
root=$(cd "$(dirname "$0")/.." && pwd)
repo=${BENCH_REPO:-SnapdragonPartners/buggrit}
remote=${BENCH_REMOTE:-origin}
run_id=${BENCH_RUN_ID:-$(date +%s)}
: "${GITHUB_TOKEN:?bench-local: GITHUB_TOKEN is not set}"
: "${BUGGRIT_DATABASE_URL:?bench-local: BUGGRIT_DATABASE_URL is not set; the results are recorded in the database}"
for tool in go git gh jq; do
  command -v "$tool" >/dev/null || { echo "bench-local: $tool not found" >&2; exit 1; }
done

say() { printf 'bench-local: %s\n' "$*" >&2; }
die() { say "$*"; exit 1; }

# The commands read these from Actions' environment. Left over in a shell,
# they'd change what's recorded: GITHUB_REPOSITORY gives the report a link
# to an Actions run that doesn't exist, and a stray label or run ID would
# file the runs under another experiment.
unset GITHUB_REPOSITORY GITHUB_SERVER_URL GITHUB_RUN_ID GITHUB_RUN_ATTEMPT \
  GITHUB_OUTPUT GITHUB_STEP_SUMMARY GITHUB_WORKSPACE GITHUB_EVENT_NAME \
  BUGGRIT_TELEMETRY_LABEL BUGGRIT_DATABASE_ERROR BUGGRIT_PR

dir=${BENCH_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/bench-local.XXXXXX")}
mkdir -p "$dir"
dir=$(cd "$dir" && pwd)
# An earlier experiment's files would break this one after its replay is
# open: prepare appends to outputs (and cleanup can't read two replays),
# and the target checkout and arm worktrees can't be made again.
[ -z "$(ls -A "$dir")" ] || die "$dir isn't empty; give BENCH_DIR a new or empty directory"
bench=$dir/buggrit-bench
say "workflow run $run_id; files in $dir"

# A key that isn't set fails only when the model is first called: for the
# judge, after every review has run.
check_keys() { # <what> <JSON>: every api_key_env in the JSON must be set
  local name
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    [ -n "${!name:-}" ] || die "$name is not set, and $1 names it"
  done < <(jq -r '.. | objects | .api_key_env? // empty' <<<"$2")
}

go build -C "$root" -o "$bench" ./cmd/buggrit-bench
"$bench" check --root "$root" "$experiment"
check_keys "the judge" "$(jq -c '.judge // {}' "$experiment")"

"$bench" prepare --root "$root" --experiment "$experiment" \
  --workflow-run "$run_id" --attempt 1 --buggrit-repo "$repo" --output "$dir/outputs"
output() { sed -n "s/^$1=//p" "$dir/outputs"; }
replay=$(output replay)
commits=$(output commits)
matrix=$(output matrix)
[ -n "$replay" ] && [ -n "$commits" ] && [ -n "$matrix" ] || die "prepare wrote no replay, commits or matrix"

# From here the replay is open: close it however the script ends.
worktrees=()
stopped=
finish() {
  local code=$?
  trap - EXIT
  "$bench" cleanup --replay "$replay" || { say "cleanup failed: close the replay by hand (docs/benchmarking.md)"; code=1; }
  local w
  for w in "${worktrees[@]+"${worktrees[@]}"}"; do
    git -C "$root" worktree remove --force "$w" || true
  done
  # BENCH_DIR started empty, so a report there is this experiment's: a
  # report whose judge failed on a run is still written.
  if [ -e "$dir/report.md" ]; then
    say "workflow run $run_id attempt 1; report in $dir/report.md"
    say "regenerate it: BUGGRIT_DATABASE_URL=... $bench report --root $root --workflow-run $run_id --attempt 1 --no-judge"
  else
    say "workflow run $run_id attempt 1: no report written"
  fi
  exit "$code"
}
trap finish EXIT
trap 'stopped=1; say "interrupted: no more runs; reporting what ran"' INT TERM

# Every arm's binary is built, and the database checked for it, before
# any model time is spent.
binary() { printf '%s/buggrit-%s' "$dir" "$1"; }
while IFS= read -r commit; do
  bin=$(binary "$commit")
  git -C "$root" fetch -q "$remote" "$commit"
  git -C "$root" worktree add -q --detach "$dir/arm-$commit" "$commit"
  worktrees+=("$dir/arm-$commit")
  # From a git checkout, so the build info carries the arm's commit
  # (telemetry's meta.mph.commit).
  go build -C "$dir/arm-$commit" -o "$bin" ./cmd/buggrit
  # As bench.yml does: `db status` exits 0 for a schema that's behind.
  status=$("$bin" db status)
  printf '%s\n' "$status" >&2
  if grep -qE 'this binary needs|dirty' <<<"$status"; then
    die "the database isn't migrated for $commit's buggrit, so its runs would record nothing"
  fi
done < <(jq -r '[.include[].commit] | unique[]' <<<"$matrix")

while IFS= read -r config; do
  check_keys "an arm's config" "$config"
done < <(jq -c '.include[].config | fromjson' <<<"$matrix")

# Every entry reviews the same replay, from a checkout of the target's head.
target_repo=$(jq -r '.include[0].repo' <<<"$matrix")
head=$(jq -r '.include[0].head' <<<"$matrix")
gh repo clone "$target_repo" "$dir/target" -- -q --no-checkout
git -C "$dir/target" fetch -q origin "$head"

count=$(jq '.include | length' <<<"$matrix")
field() { jq -r ".$1" <<<"$entry"; }
for ((i = 0; i < count; i++)); do
  [ -z "$stopped" ] || break
  entry=$(jq -c ".include[$i]" <<<"$matrix")
  arm=$(field arm) run=$(field run)
  say "review $arm $run ($((i + 1)) of $count)"
  git -C "$dir/target" checkout -q -f --detach "$head"
  git -C "$dir/target" clean -qfdx
  field config >"$dir/config-$arm.json"
  # An incomplete pass exits 1; that's a result to report, not a failed
  # run, so the exit code is ignored and the telemetry says how it ended.
  GITHUB_RUN_ID=$run_id GITHUB_RUN_ATTEMPT=1 BUGGRIT_TELEMETRY_LABEL=$(field label) \
    "$(binary "$(field commit)")" review --dry-run --repo "$(field repo)" --pr "$(field pr)" \
    --config "$dir/config-$arm.json" --workspace "$dir/target" 2>&1 |
    tee -i "$dir/review-$arm-$run.log" ||
    say "the review exited ${PIPESTATUS[0]}; the report says how the pass ended"
done

# Report what ran, even after an interrupt. A second one stops the report
# (buggrit-bench cancels), and the replay is still closed.
trap - INT TERM
"$bench" report --root "$root" --experiment "$experiment" --replay "$replay" --commits "$commits" \
  --workflow-run "$run_id" --attempt 1 --out "$dir/report.md"
[ -z "$stopped" ] || exit 130
