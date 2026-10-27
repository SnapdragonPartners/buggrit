# ADR 0002: The releaser moves `v0`, not a workflow

## Status

Accepted (Dan, 2026-10-27). Supersedes how [ADR 0001](0001-immutable-release-references.md) decision 4 moved `v0`. The rest of ADR 0001 stands.

## Date

2026-10-27

## Context

Under ADR 0001, `release.yml`'s `major` job moved `v0` to each new release with a buggrit-bot App token, and the App was on the "Release tags" ruleset's bypass list. The first release made that way, v0.5.0 (run 40567834779), published fine, but the `major` job's push was refused:

> refusing to allow a GitHub App to create or update workflow `.github/workflows/review.yml` without `workflows` permission

The ruleset let the App through. GitHub's separate check on workflow files compares what a ref pointed at before and after the push, even when both commits already exist. Moving `v0` from v0.4.0 to v0.5.0's release commit changes what `review.yml` says at `v0`. Every release changes it at least in the pinned `uses:` lines, so this would fail on every release.

ADR 0001 had taken the earlier releases as evidence that this check doesn't apply. `GITHUB_TOKEN` had moved `v0` across workflow changes from v0.3.0 to v0.4.0. That held for `GITHUB_TOKEN`, but not for App tokens, which are what the ruleset needs, since `GITHUB_TOKEN` can't be on a bypass list.

## Decision

1. **`make release` moves `v0`, with the releaser's credentials.** After pushing the release tag, `scripts/release.sh` waits, up to 20 minutes by default, until the release is published with its assets. Then it runs `scripts/move-majors.sh`, which moves each major tag to its major's newest published final release. That's the `major` job's logic, unchanged.
2. **The wait matters.** `@v0` callers install the release at `v0`'s commit, and a release still building has no assets, so `v0` moves only once the release is published.
3. **`make move-majors` finishes the job by hand** if `make release` stopped before moving `v0`. It reads the published releases fresh, so running it twice, or late, is harmless.
4. **`release.yml` has no `major` job, and the ruleset bypasses only admins.** The buggrit-bot App comes off the bypass list: nothing in a workflow creates or moves `v*` tags any more.

## Consequences

- A release takes the releaser's terminal until it's published, a few minutes. The pre-push hook also runs twice, once for the tag and once for `v0`.
- If the releaser stops early, `v0` lags until someone runs `make move-majors`. Pinned callers aren't affected, and `@v0` callers keep the previous release, which is safe.
- Fewer actors can move release tags: admins only. The org-wide App key no longer can, which removes the trust-model caveat in ADR 0001's consequences.
- v0.5.0's `v0` was moved by hand after the failed run, the same way `move-majors.sh` does it.

## Alternatives considered

- **Give buggrit-bot the Workflows permission.** Already rejected in ADR 0001: every token minted from the org-wide key, each review's included, could then edit workflows in every repository the App is installed on.
- **A dedicated release App** (Contents and Workflows write, installed only on buggrit, its key a buggrit-only secret behind a tag-restricted environment, on the bypass list). It keeps `v0` automatic, but it's a new App, key and environment to manage, all to save the releaser a few minutes of waiting.
- **Stop moving `v0`.** It's simplest, but morris and maestro-llms call `@v0` today and would stay on v0.4.0 until their pin PRs land. It can still come later, once no caller uses `@v0`.
- **Push `v0` together with the release tag.** For the few minutes while the release builds, `@v0` callers would fail to install.

## Follow-up triggers

- Once no caller uses `@v0`, reconsider whether to move it at all.
- GitHub lets an App move a tag across workflow changes without the `workflows` permission, or lets `GITHUB_TOKEN` bypass rulesets.
