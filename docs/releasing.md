# Releasing buggrit

How a buggrit release is made, why it's made this way, and the repository
settings it relies on. [ADR 0001](adr/0001-immutable-release-references.md)
records the decision and the alternatives;
[design §3](design.md#3-trigger-packaging-permissions) ("Releases", "Which
binary") is the binding summary; this is the runbook.

## Why a release isn't a tag on `main`

A caller runs buggrit through the reusable workflow `review.yml`, which
runs two of buggrit's own actions: `.github/actions/database` and the
composite action at the repository root. A `uses:` line can't be an
expression, so `review.yml` has to name a ref for them in its text. On
`main` that ref is `@v0`, the moving tag.

If a release kept `@v0`, pinning the workflow wouldn't pin anything: a
caller at `review.yml@<sha>` would still run whatever `v0` points at for the
steps that hold its tokens, including its GitHub OIDC token (the job has
`id-token: write`). So each release gets its own commit, made on top of a
`main` commit, whose only change is that `review.yml`'s own `uses:` lines
name the release:

```
main:     A ── B ── C
                     \
release:              R   (tag v0.5.0)
                          review.yml: uses: SnapdragonPartners/buggrit@v0.5.0
                                      uses: SnapdragonPartners/buggrit/.github/actions/database@v0.5.0
```

`R` isn't on any branch; the tag keeps it. A caller that pins
`review.yml@<R's SHA>` runs `R`'s workflow, `R`'s actions (through the
`v0.5.0` tag, which points at `R`) and the `v0.5.0` binary, and nothing
else. `v0` moves to `R` as well, so `@v0` callers get the same consistent
set.

The nested references name the release's tag, not a SHA: a commit can't
contain its own SHA. The tag is as fixed as a SHA here, because immutable
releases (below) lock a published release's tag to its commit, and its
assets with it. The binary comes from that same release either way.

### Why a script, not a workflow

`R` changes a file under `.github/workflows/`. GitHub refuses a push that
creates or changes workflow files unless the token has the `workflows`
permission, and the Actions `GITHUB_TOKEN` can never have it. Granting it to
the buggrit-bot App would let every token minted from the org-wide App key,
including each review's, edit workflows in every repository the App is
installed on. So the release commit is made and pushed from a releaser's
machine, with their own credentials, and the workflow checks it.

## One-time setup

Done once, by a repository admin. `make release` refuses to run until both
are in place.

1. **Immutable releases.** Settings → General → Releases → "Enable release
   immutability", or:

   ```sh
   gh api -X PUT repos/SnapdragonPartners/buggrit/immutable-releases
   gh api repos/SnapdragonPartners/buggrit/immutable-releases   # {"enabled":true,...}
   ```

   A release published after this can't have its assets changed, and its
   tag can't be moved or deleted; even after the release is deleted, the tag
   name can't be reused. Releases published before it (v0.1.0–v0.4.0) stay
   mutable. goreleaser creates each release as a draft, uploads the assets,
   then publishes it, which is the order immutable releases need.

2. **The "Release tags" ruleset.** It blocks creating, updating and deleting
   `v*` tags, except for repository and organization admins:

   ```sh
   gh api -X POST repos/SnapdragonPartners/buggrit/rulesets --input - <<'EOF'
   {
     "name": "Release tags",
     "target": "tag",
     "enforcement": "active",
     "conditions": { "ref_name": { "include": ["refs/tags/v*"], "exclude": [] } },
     "rules": [{ "type": "creation" }, { "type": "update" }, { "type": "deletion" }],
     "bypass_actors": [
       { "actor_type": "RepositoryRole", "actor_id": 5, "bypass_mode": "always" },
       { "actor_type": "OrganizationAdmin", "actor_id": 1, "bypass_mode": "always" }
     ]
   }
   EOF
   ```

   (`RepositoryRole` 5 is the admin role.) No App or workflow token is on
   the list: nothing in a workflow may create or move these tags.
   [ADR 0002](adr/0002-the-releaser-moves-v0.md) says why `v0` moves with
   the releaser's credentials too.

## Cutting a release

1. Close every open issue labeled `release-blocker`
   (`gh issue list --label release-blocker`).
2. Check out `main`, pull, and make sure CI is green on it. You need to be
   an admin of the repository (an org owner is one): the ruleset lets
   nobody else create `v*` tags.
3. Run:

   ```sh
   make release RELEASE=v0.5.0
   ```

   `scripts/release.sh` stops, changing nothing, unless:
   - the version is `vX.Y.Z` (or a pre-release, `vX.Y.Z-rc.1`);
   - the worktree is clean and `HEAD` is `origin/main`;
   - the tag doesn't exist locally or on `origin`;
   - no `release-blocker` issue is open;
   - immutable releases are on and the "Release tags" ruleset is active;
   - you're an admin of the repository, so the ruleset lets your push
     through.

   It then writes `R` in a throwaway worktree (`scripts/pin-release.sh`
   rewrites `review.yml`), tags it locally, checks it with
   `scripts/verify-release.sh` exactly as `release.yml` will, and shows the
   commit and its diff. It asks before pushing. Answer `y` to push the tag.
   Any other answer, no terminal to ask on, or a failed push deletes the
   local tag, so a rerun starts clean. After a failed push, check whether
   the tag reached `origin` (`git ls-remote --tags origin`); the rerun
   refuses if it did. The pre-push hook runs lint and
   tests on the push, as for any push.
4. The tag push starts `release.yml` (`gh run list --workflow release.yml`).
   It checks out the tag and runs `scripts/verify-release.sh`: a tag that
   isn't exactly a `main` commit plus the pinned `review.yml` fails here,
   before anything is built. Then it runs the tests and goreleaser, which
   publishes the release with the four archives and `checksums.txt`.
5. Meanwhile the script waits, up to 20 minutes, for the release to be
   published with its assets. Then it moves each major tag (`v0`) to its
   newest published final release (`scripts/move-majors.sh`), with your
   credentials; the pre-push hook runs again for that push. It waits because
   `@v0` callers install the release at `v0`'s commit, and a release still
   building has nothing to install. A workflow can't move `v0`: the move
   changes what `review.yml` says at `v0`, and GitHub refuses that from any
   token without the `workflows` permission, App tokens included
   ([ADR 0002](adr/0002-the-releaser-moves-v0.md)).
6. Check:

   ```sh
   gh release view v0.5.0                       # four archives + checksums.txt
   gh api repos/SnapdragonPartners/buggrit/releases/tags/v0.5.0 --jq .immutable   # true
   git fetch --tags --force && git rev-parse v0 v0.5.0   # the same commit
   ```

A pre-release (`v0.5.0-rc.1`) works the same way, except that `v0` doesn't
move to it.

## When something goes wrong

- **`make release` refused.** It changed nothing and left no tag; fix what
  it says and run it again.
- **`release.yml` failed before the release was published** (a test, a
  goreleaser error, a GitHub outage). Delete the draft release goreleaser
  left, if there is one, and re-run the failed jobs. If the code itself needs a fix, the tag
  can't be reused: delete the draft release if there is one, and cut the
  next patch version from the fixed `main`. (An admin can delete an
  unpublished tag through the ruleset bypass, but a new version is simpler
  and leaves no doubt about what a tag means.)
- **`verify-release.sh` failed in `release.yml`.** The tag wasn't made by
  `make release` (most likely `main`'s own commit tagged by hand), or
  `main` was rewritten under it. Nothing was published. The tag can't be
  reused by anyone but an admin; cut the next version with `make release`.
  The check catches mistakes, not intent: the workflow runs from the tagged
  commit, whose `release.yml` could leave the check out. The ruleset, which
  lets only admins create `v*` tags, is the control there.
- **The release is published but broken.** It can't be changed. Cut the
  next patch; `v0` moves to it. Callers pinned to the broken release move
  when Dependabot bumps them, or by hand.
- **`make release` stopped before moving `v0`** (the wait ran out, the
  terminal closed, or the push failed). The release is out, or will be once
  `release.yml` finishes, but `v0` didn't move. Once the release is
  published, run `make move-majors`; it reads the published releases
  fresh, so running it again, or late, is harmless.

## For callers

Pin the reusable workflow to a release commit's SHA, with the version in a
comment so Dependabot can bump it (README, "Pinning"):

```yaml
jobs:
  buggrit:
    uses: SnapdragonPartners/buggrit/.github/workflows/review.yml@<SHA of v0.5.0's commit> # v0.5.0
    secrets: inherit
```

`git ls-remote https://github.com/SnapdragonPartners/buggrit refs/tags/v0.5.0`
prints the SHA. Pin v0.5.0 or later: earlier releases' `review.yml` still
uses `@v0` for its own actions.
