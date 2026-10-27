# ADR 0001: A release pins its own actions, and release tags are protected

## Status

Accepted (Dan, 2026-10-27). Resolves [#44](https://github.com/SnapdragonPartners/buggrit/issues/44), which buggrit's review of morris [#516](https://github.com/SnapdragonPartners/morris/pull/516) raised (threads f-2b7c86, f-762b10). morris's side of the same finding is its ADR 0057.

## Date

2026-10-27

## Context

Other repositories run buggrit through the reusable workflow `review.yml`, which runs two of buggrit's own actions: `.github/actions/database` and the composite action at the repository root. A `uses:` line can't be an expression, so `review.yml` names their ref in its text, and until now that ref was `@v0`, the moving tag.

So pinning the workflow pinned nothing that mattered. A caller at `review.yml@<sha>` still ran whatever `v0` pointed at for the steps that hold its tokens. Since v0.4.0 the review job has `id-token: write`, so those steps can mint the caller's GitHub OIDC token. On morris that token could log in as its deploy identity until morris ADR 0057 narrowed that grant. buggrit also had no tag ruleset, so anyone with write access to buggrit, or a stolen token for one of them, could move `v0` or a `vX.Y.Z` tag. Third-party actions in the same job (`actions/checkout`, `actions/create-github-app-token`, `google-github-actions/auth`) were also pinned by tag.

## Decision

1. **A release is its own commit.** Releasing `vX.Y.Z` makes a commit on top of a `main` commit whose only change is that `review.yml`'s own `uses:` lines say `@vX.Y.Z` instead of `@v0` (`scripts/pin-release.sh`). The tag points at that commit, which is on no branch. A caller pinning `review.yml` to the release's SHA runs that release's workflow, actions and binary. `main` keeps `@v0`.
2. **The nested references name the release's tag, not a SHA**, since a commit can't contain its own SHA. The repository has **immutable releases** on, so a published release's tag can't move or be deleted, its assets can't change, and its name can't be reused. That makes `@vX.Y.Z` as fixed as a SHA, and the binary comes from the same release either way.
3. **The release commit is made on the releaser's machine.** `make release RELEASE=vX.Y.Z` (`scripts/release.sh`) writes the commit and pushes the tag with the releaser's credentials. The tag push runs `release.yml` as before. The script refuses while a `release-blocker` issue is open, while the protections in 2 and 4 are off, or unless it runs from a clean, current `main`.
4. **A tag ruleset, "Release tags", covers `refs/tags/v*`.** It blocks creating, moving and deleting those tags for everyone except repository and organization admins and the buggrit-bot App. `v0` keeps moving, as the convenience option: `release.yml`'s `major` job moves it with a buggrit-bot App token.
5. **`release.yml` checks the tag first** (`scripts/verify-release.sh`): exactly one commit, whose parent is on `main` and whose tree is that parent's plus the rewrite. Otherwise it publishes nothing. This catches mistakes such as `main` tagged by hand. It's not the control against a hostile tag, because the workflow runs from the tagged commit's own copy, which could leave the check out. The ruleset in 4 is that control.
6. **Every third-party action in buggrit's workflows and actions is pinned by commit SHA**, with its version in a comment. Dependabot bumps both, the database action's directory included. buggrit's own actions in `review.yml` are the exception, under 1 and 2: `@v0` on `main`, the release's tag in a release.
7. **Callers pin by SHA** (`review.yml@<sha> # vX.Y.Z`, bumped by Dependabot). Repositories whose cloud accepts GitHub OIDC tokens grant each identity on `workflow_ref`/`job_workflow_ref` or a protected environment, never on a repository or branch alone, and pin by SHA every action in jobs that log in (design §10).

## Consequences

- Releasing changes from pushing a tag on `main` to `make release`. A tag pushed on `main` by hand fails in `release.yml` and publishes nothing; the simplest fix is the next version. [docs/releasing.md](../releasing.md) is the runbook, recovery included.
- A published release can't be fixed in place: a broken one is followed by a patch release.
- Only admins can cut releases, since only they can create `v*` tags. The ruleset depends on the buggrit-bot App staying on its bypass list. Anyone holding the org-wide App key can therefore move `v0`, which is within the trust model (design §1). Pinned callers aren't affected.
- Releases before v0.5.0 still use `@v0` for their own actions; callers pin v0.5.0 or later.
- `v0` points at a commit that isn't on `main`. `git log main` doesn't show releases; `git tag` and the releases page do.
- SHA pins make action bumps Dependabot PRs rather than silent tag moves. That costs more PRs, and each bump gets reviewed.

## Alternatives considered

- **A workflow (dispatch) makes the release commit.** The commit changes a file under `.github/workflows/`, and GitHub refuses that push from any token without the `workflows` permission. `GITHUB_TOKEN` can never have it. Adding it to buggrit-bot would let every token minted from the org-wide key, each review's included, edit workflows in every repository the App is installed on.
- **Nested references by SHA**: the parent `main` commit's SHA, with the binary version written into the rewritten default. The action code would be pinned by SHA, but the binary would still come from the release, and `install.sh` would need another resolution path. With immutable releases the tag is equivalent.
- **Tag `main` as `vX.Y.Z`, and put the release commit under a second tag** (`vX.Y.Z-wf`). Callers and Dependabot would have to track the unusual tag.
- **Keep `@v0` and have callers pass `version`.** That pins only the binary, not the action code that holds the tokens, which was the gap in the first place.
- **Protect only `vX.Y.Z` and leave `v0` unprotected.** It's simpler, but anyone with write access could move the tag most callers use today.

## Follow-up triggers

- buggrit's workflow starts using another of buggrit's own actions: `pin-release.sh` rewrites every `SnapdragonPartners/buggrit…@v0` `uses:` line in `review.yml`, but only that file.
- A second major version (`v1`): the `major` job already moves every major tag; check the ruleset pattern still covers it.
- The buggrit-bot App is replaced or renamed: update the ruleset's bypass list.
- GitHub lets `GITHUB_TOKEN` push workflow changes or act as a bypass actor: reconsider decisions 3 and 4.
