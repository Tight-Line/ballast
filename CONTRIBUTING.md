# Contributing to Ballast

Thanks for your interest in contributing. This document covers the essentials.

## What we welcome

- **Bug fixes** — Include a test case that fails before your fix and passes after.
- **New metrics plugins** — Implement the `MetricsPlugin` interface for a new backend
  (Prometheus, VictoriaMetrics, etc.). Open an issue first to align on the design.
- **Performance improvements** — Include benchmarks showing the improvement.
- **New features** — Open an issue first to discuss the design before writing code.

## Before you open a PR

1. Run `make check` locally and confirm it passes (`lint` + 100% coverage + build).
2. Update `CHANGELOG.md` under `[Unreleased]` for any user-visible change.
3. Update `AGENTS.md` if your change adds files, moves key functions, or changes the
   build/test workflow.
4. If your change affects the Helm chart, run `helm lint charts/ballast` as well.

## Pull request checklist

- [ ] Linked to the relevant issue (use `Fixes #NNN` or `Relates to #NNN`)
- [ ] Tests added or updated
- [ ] `make check` passes locally
- [ ] `helm lint charts/ballast` passes (if chart was changed)
- [ ] `CHANGELOG.md` updated
- [ ] Documentation updated if behavior or configuration changed

## A note on AI-assisted contributions

We use AI tools in our own development and welcome others who do the same. However,
PRs must demonstrate human understanding of the changes. Include clear motivation
explaining *why* the change is needed, not just what it does. Explain your testing
approach. Low-effort submissions that appear to be unreviewed AI output will be declined.
We value quality over quantity.

## Reporting bugs

Open a [bug report](https://github.com/tight-line/ballast/issues/new?template=bug_report.md).

## Suggesting features

Open a [feature request](https://github.com/tight-line/ballast/issues/new?template=feature_request.md).

## Security issues

Please do **not** open public issues for security vulnerabilities.
See [SECURITY.md](SECURITY.md) for responsible disclosure instructions.
