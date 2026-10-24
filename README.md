# buggrit

An automated pull-request reviewer that runs as a GitHub Action. Like Copilot code review, it posts each finding as its own review thread. Unlike Copilot, it:

- works with any model [maestro-llms](https://github.com/SnapdragonPartners/maestro-llms) supports, including self-hosted and flat-rate models;
- re-verifies its earlier findings on every push and reply, and resolves, reopens or escalates their threads;
- is built to make the review → fix → review loop with a coding agent end in an explicit final state.

> [!WARNING]
> **Pre-release: not ready for general use.** buggrit is in early development and is being piloted on a few SnapdragonPartners repositories. Its behavior, configuration and workflow interface may change without notice between `v0` releases, and the convergence controls the design depends on (build step 5) aren't built yet. It assumes trusted repositories ([design §1](docs/design.md#1-goals-and-context)). On a private repository anyone who can comment counts as a human reviewer; on a public one only the team does (members, collaborators and anyone with write access), and fork PRs are skipped. We aren't taking outside issues or contributions yet.

The design is in [docs/design.md](docs/design.md). Contributor workflow is in [CLAUDE.md](CLAUDE.md).

## Using buggrit in a repository

For SnapdragonPartners repositories, while buggrit is in pilot.

**Once per org** (done for SnapdragonPartners):

- buggrit → Settings → Actions → General → Access: "Accessible from repositories in the SnapdragonPartners organization". This covers the action and the reusable workflow.
- The `buggrit-bot` GitHub App installed on buggrit (to download release assets) and on each repository it reviews. Permissions: Pull requests, Contents and Checks, all read and write ([design §3](docs/design.md#3-trigger-packaging-permissions)).
- The org variable `BUGGRIT_APP_ID` and the secrets `BUGGRIT_APP_PRIVATE_KEY` and `LUNAROUTE_API_KEY` visible to the repository.

**Per repository**, add `.github/workflows/buggrit.yml`:

```yaml
name: buggrit
on:
  pull_request:
    types: [opened, synchronize, reopened, ready_for_review]
  pull_request_review_comment:
    types: [created]
  pull_request_review:
    types: [submitted]
  workflow_dispatch:
    inputs:
      pr:
        description: Pull request number to review
        required: true
concurrency:
  # buggrit's own comments get a throwaway group, so they never replace a
  # run queued by a human. The login must match identity.bot_login.
  group: >-
    ${{ (github.event.comment.user.login == (vars.BUGGRIT_BOT_LOGIN || 'buggrit-bot[bot]') ||
         github.event.review.user.login == (vars.BUGGRIT_BOT_LOGIN || 'buggrit-bot[bot]'))
        && format('buggrit-ignored-{0}', github.run_id)
        || format('buggrit-{0}', github.event.pull_request.number || inputs.pr) }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
permissions:
  contents: read
jobs:
  buggrit:
    uses: SnapdragonPartners/buggrit/.github/workflows/review.yml@v0
    secrets: inherit
```

and a minimal `.github/buggrit.json`:

```json
{
  "version": 1,
  "models": {
    "default": {
      "provider": "openai-compatible",
      "base_url": "https://gw.lunaroute.com/v1",
      "model": "deepseek-4.1-flash",
      "api_key_env": "LUNAROUTE_API_KEY",
      "max_output_tokens": 32768,
      "reasoning_effort": "medium"
    }
  },
  "limits": { "pass_deadline": "55m", "max_concurrency": 1 }
}
```

buggrit reads the config from the PR's base branch, so it starts reviewing once the config is merged. Check a config with `buggrit config validate`; [design §4](docs/design.md#4-configuration) has every field.

- **Pinning:** `@v0` follows the newest v0 release. For a fixed release, pass `with: { version: v0.1.0 }`, or pin both the workflow and `version`.
- **Other inputs:** `runs-on` (default `ubuntu-latest`; the runner must reach the model endpoint, and a self-hosted one needs Actions Runner v2.327.1 or later, plus `NODE_USE_ENV_PROXY=1` behind a proxy) and `timeout-minutes` (default 70; keep it above `pass_deadline`).
- **A different bot login:** set the variable `BUGGRIT_BOT_LOGIN` and `identity.bot_login` in the config to the same value.
- **The check** to wait on is `buggrit / review`. On a public repository, a run started by a reply or review whose author shows as anything but `OWNER`, `MEMBER` or `COLLABORATOR` reports as `buggrit / outside event` instead, even when buggrit reviews for real because the author has write access. `gh pr checks --watch` waits on both; an agent that waits on a check by name should accept either ([design §3](docs/design.md#3-trigger-packaging-permissions), "Outside events").

## Development

```sh
make deps          # check required tools
make check         # lint, vulnerability check, build, test
make install-hooks # pre-push: lint + test
```

Check a config file:

```sh
go run ./cmd/buggrit config validate path/to/buggrit.json
```

## License

[MIT](LICENSE)
