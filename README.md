# buggrit

An automated pull-request reviewer that runs as a GitHub Action. Like Copilot code review, it posts each finding as its own review thread. Unlike Copilot, it:

- works with any model [maestro-llms](https://github.com/SnapdragonPartners/maestro-llms) supports, including self-hosted and flat-rate models;
- re-verifies its earlier findings on every push, and resolves, reopens or escalates their threads;
- is built to make the review → fix → review loop with a coding agent end in an explicit final state.

**Status:** early development. The design is in [docs/design.md](docs/design.md). Contributor workflow is in [CLAUDE.md](CLAUDE.md).

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
