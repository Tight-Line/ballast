# Ballast — Claude Code Instructions

## Lint + coverage gate: required before every commit

A commit is not complete until all of these pass:

1. `git status` — every file touched by the fix must be staged. `make lint` and `make test-coverage-check` both pass locally when edits are on disk even if they are not staged; CI sees only what is committed.
2. `make lint` — runs golangci-lint; fix all issues before committing. Common traps:
   - American spellings required (`serializes` not `serialises`, `initialize` not `initialise`, etc.)
   - Named return values required when gocritic flags `unnamedResult`
   - Remove empty `if` branches (staticcheck SA9003)
2. `make test-coverage-check` — runs `make check` internally; coverage gate must be green.

When a line is uncovered: write a test for it first. Use `// coverage:ignore - <reason>` only when testing is genuinely impossible (e.g. `json.Marshal` on a well-typed struct, transient API errors that require a broken client). Do not use `// coverage:ignore` as a first resort — it defeats the purpose of the coverage gate.
