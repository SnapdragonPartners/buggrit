# Diff fixtures

`git.diff` is real `git diff --cached -M` output (with `core.quotepath=true`)
from a scratch repository, covering added, deleted, modified (multi-hunk),
renamed (with and without changes), binary, mode-only, no-newline-at-EOF,
quoted non-ASCII and space-containing paths. `git.golden.json` is the parsed
result plus the anchors computed from it; regenerate it with
`go test ./internal/github -run TestDiffGolden -update` and review the change.
