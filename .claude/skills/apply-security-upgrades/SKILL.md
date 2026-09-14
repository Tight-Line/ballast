---
name: apply-security-upgrades
description: Roll every outstanding Dependabot PR into one branch and get Snyk, SonarCloud, govulncheck, lint and the coverage gate green. Use periodically to clear the dependency backlog, or when Dependabot PRs are piling up red.
---

# Apply security upgrades

Collapse the open Dependabot PRs into a single reviewable branch, then fix
whatever the security and quality gates report. One branch, one PR, one CI run
instead of five that each burn a full matrix.

## Why a single branch

Dependabot opens one PR per ecosystem group. Each one runs the whole CI matrix,
each one has to be merged and then rebased against the others, and none of them
can fix a problem that lives outside its own diff, such as a stale `go` directive
or a missing egress endpoint. Merging them together first means one CI run, one
review, and a place to put the fixes that no individual bump could carry.

## Procedure

### 1. Survey

```bash
gh pr list --limit 50 --json number,title,headRefName,author \
  --jq '.[] | select(.author.login=="app/dependabot") | "\(.number)\t\(.headRefName)\t\(.title)"'
```

For each one, check it is a forward bump and not stale; main may already carry a
newer bump of the same group, in which case the PR is superseded:

```bash
git fetch origin
git diff origin/main...origin/<branch>
```

Also capture the current gate status, and **capture it for `main` too**:

```bash
gh pr checks <n>
gh run list --branch main --limit 15 \
  --json conclusion,name,headSha,createdAt \
  --jq '.[] | "\(.conclusion)\t\(.name)\t\(.headSha[0:8])"'
```

> If a check is red on *every* PR including ones that touch only the Dockerfile,
> it is not the dependency bumps. It is infrastructure, and it is almost
> certainly also red on `main`. Diagnose it before touching any dependency.

### 2. Build the branch

```bash
git checkout -b chore/security-upgrades-<YYYY-MM> origin/main
```

**Cherry-pick; do not merge.** `main` has a strictly linear history and the repo
is merged with GitHub's "Rebase and merge", which replays commits individually and
throws merge commits away. A branch built with `git merge` therefore fails with
`This branch cannot be rebased due to conflicts` even when GitHub simultaneously
reports the PR as `MERGEABLE` and `CLEAN`: the conflict resolution you did lives
*inside* a merge commit, rebase discards it, and the conflict comes back with
nothing to resolve it. Cherry-picking resolves the conflict inline, in a real
commit, where rebase can carry it.

Apply in a deliberate order; cheapest and least conflict-prone first, Go modules
last, and within Go modules the **grouped minor/patch bump before the security
bump** so the security pin is the one that survives:

1. `github-actions` group
2. `docker` bumps
3. `gomod` minor/patch group
4. `gomod` security group

```bash
# each Dependabot branch is a single commit; -x records where it came from
git cherry-pick -x origin/<branch>
```

Cherry-pick preserves `dependabot[bot]` as the author, so the audit trail survives.
Merging this PR still auto-closes the Dependabot PRs, because the PR body's
`Closes #NNN` keywords do that regardless of whether their exact SHAs land.

If a branch is already built with merges, linearize it before pushing rather than
reaching for squash:

```bash
git branch backup/pre-rebase-$(git rev-parse --short HEAD)   # recoverable
git rebase origin/main                                       # resolve once, see below
git diff backup/pre-rebase-<sha> HEAD                        # confirm the tree
```

### 3. Resolve go.mod / go.sum conflicts

The two Go module changes will conflict when the security bump goes on top of the
grouped one. Do **not** hand-merge the version lines. Take the already-applied
side, then re-apply the incoming pin through the toolchain so `go.sum` stays
internally consistent:

```bash
git checkout --ours go.mod go.sum
go get <module>@<version>   # re-apply each security pin the incoming commit carried
go mod tidy                 # slow; run it in the background
git add go.mod go.sum
git cherry-pick --continue  # or `git rebase --continue`
```

`--ours` means "what is already applied" during both cherry-pick and rebase, which
is the grouped bump. That is the side to keep. Note this is the opposite sense
from `git merge`, where `--ours` is the branch you are merging into.

Rule for each conflicting line: **take the higher version**. The minor/patch
group is often newer for transitive deps such as `genproto` and `protobuf`, while
the security group is newer for the one module it targets, such as `grpc`. Both
need to win where they are ahead.

`go mod tidy` may legitimately prune `go.sum` lines for the version being replaced.
That is correct, not drift. But it means the tree is no longer byte-identical to a
previously verified one, so re-run the gate rather than trusting the earlier
green run.

### 4. Fix the gates

Run the repo gate locally before pushing. It is required before every commit; see
`CLAUDE.md`.

```bash
make check            # lint + test-coverage-check + build
go tool govulncheck ./...
```

Both are slow. Run them in the background and **read the real output**, not a
wrapper's exit status. `govulncheck` exits `3` on findings, and a trailing
`echo EXIT=$?` through a pipe reports the pipe's status rather than the tool's.

Known classes of problem, in the order they tend to bite:

#### The stale `go` directive (Dependabot cannot fix this)

`govulncheck` reports Go **standard library** vulnerabilities against the version
in the `go` directive of `go.mod`. Dependabot bumps modules and the Dockerfile
builder image; it never bumps that directive. So the stdlib quietly rots and
`govulncheck` fails with findings that no dependency PR can clear.

```bash
# what patch releases exist on the current minor line
curl -s "https://go.dev/dl/?mode=json&include=all" \
  | grep -oE '"version": "go1\.[0-9]+\.[0-9]+"' | sort -u -V | tail
# setup-go must be able to install it
curl -s "https://raw.githubusercontent.com/actions/go-versions/main/versions-manifest.json" \
  | grep -oE '"version": "1\.[0-9]+\.[0-9]+"' | sort -u -V | tail
```

Bump the `go` directive to the **latest patch on the current minor line**, not to
a new minor. A minor bump changes language semantics and vet/lint behavior and
does not belong in a security pass. Precedent: commit `84ffc12` bumped
`1.26.0` to `1.26.5`, and the 2026-09 pass bumped `1.26.5` to `1.26.8`, both to
clear stdlib findings.

```bash
go mod edit -go=<version>    # does not add a `toolchain` directive; keep it that way
go tool govulncheck ./...    # must print "No vulnerabilities found."
```

Confirm the bump against the real output. A local toolchain older than the new
directive is downloaded automatically, so a passing run also proves the toolchain
resolved.

The Dockerfile builder image and the `go` directive do **not** need to match. A
newer toolchain building an older-directive module is fine and normal.

#### Snyk or Sonar red for reasons that are not findings

Check *why* the job is red before hunting for vulnerabilities. A scanner that
dies in setup reports the same red X as a scanner that found a critical CVE. In
the 2026-09 pass, "Snyk Security" had been red on `main` for weeks and had never
actually scanned anything.

```bash
RUN=$(gh run list --workflow=snyk.yml --branch main --limit 1 --json databaseId --jq '.[0].databaseId')
gh run view "$RUN" --log > /tmp/snyk.log
grep -nE "##\[error\]|Acquiring|ECONNREFUSED|severity|issues found" /tmp/snyk.log
```

`gh run view --log-failed` is often useless here, and so is `tail`: harden-runner
dumps its entire agent log into the job's teardown, so both show you eBPF and
systemd chatter instead of the error. Find the failing step by name, then grep
only that step's lines:

```bash
gh run view <run_id> --json jobs \
  --jq '.jobs[] | {name, conclusion, failed: [.steps[]|select(.conclusion=="failure")|.name]}'
JOB=<databaseId from above>
gh run view --job "$JOB" --log | grep -F "<failing step name>" | cut -c1-200 | tail -40
```

Then widen the `cut` on the one interesting line, because the real cause is often
past column 200. A truncated `unable to download ...` turned out to end in
`got status "504 Gateway Timeout"`, which is a transient, not a bug.

#### harden-runner egress blocks (the recurring one)

Every workflow runs `step-security/harden-runner` with `egress-policy: block` and
an explicit `allowed-endpoints` list. A missing endpoint fails the job with a
bare `connect ECONNREFUSED <ip>:443` and nothing else.

These failures are **latent**. The blocked download usually only happens on a
cache miss or a version change, so an allowlist can be wrong for weeks and pass
anyway, then break everything at once:

- `release-assets.githubusercontent.com:443` is where `actions/setup-go` fetches
  the Go toolchain. It is only downloaded when the runner image does not already
  ship the version in `go.mod`, **so bumping the `go` directive can break every
  job whose allowlist is missing this**. Bump the directive and the endpoint in
  the same change.
- `binaries.sonarsource.com:443` is the Sonar Scanner CLI zip, cached under a
  version-keyed Actions cache, so it only downloads when that cache is cold.
- `vuln.go.dev:443` is govulncheck's database.
- `api.snyk.io:443` is Snyk.

When a job fails at `Set up Go`, compare its allowlist against a job in the same
repo that passes. The passing job's list is the answer.

For a denial that is not an obvious toolchain fetch, get the real egress data
from the StepSecurity run page rather than the Actions log, which does not carry
it: `https://app.stepsecurity.io/github/Tight-Line/ballast/actions/runs/<run_id>`.
The run page lists every destination the job actually reached and which ones were
blocked, which is how the existing allowlists were derived. The StepSecurity
GitHub App is installed on this repo and emails on block denials.

Wildcard rule when adding an entry: wildcard only narrow vendor-owned domains
such as `*.ingest.us.sentry.io`; pin shared multi-tenant infrastructure such as
S3 and GCS buckets to the exact host. Matching is exact-FQDN, so `github.com`
does not cover its subdomains.

Audit every job at once rather than fixing them one red check at a time. System
`python3` here has no `yaml` module; `ruby -ryaml` does:

```bash
ruby -ryaml -e '
Dir[".github/workflows/*.yml"].sort.each do |f|
  y = YAML.load_file(f)
  (y["jobs"]||{}).each do |job, cfg|
    steps = cfg["steps"]||[]
    next unless steps.any?{|s| s["uses"].to_s.include?("actions/setup-go")}
    hr = steps.find{|s| s["uses"].to_s.include?("harden-runner")}
    eps = hr ? hr["with"]["allowed-endpoints"].to_s.split : []
    ok = eps.include?("release-assets.githubusercontent.com:443")
    puts "%-34s %s" % [File.basename(f)+"/"+job, ok ? "ok" : "*** MISSING ***"]
  end
end'
```

Jobs that never invoke `setup-go` but still run `go build`, such as CodeQL
autobuild and the image builds, pull a bumped toolchain through the module proxy
instead, so they need `proxy.golang.org:443` rather than `release-assets`. Swap
the `setup-go` test above for that endpoint to check them.

Adding an endpoint to an allowlist may be refused by the permission classifier as
a "security weaken" when done through a scripted `sed` or `python` edit. Make the
edit with the normal file-edit tool instead, so the change is explicit and
reviewable.

#### Sonar quality gate

Sonar only fails on **new code**. Dependency bumps rarely move it, but a `go mod
tidy` that drops or adds a file can. If it fails, read the gate on the PR
decoration rather than guessing. Coverage is fed from `coverage.filtered.out`
produced by `make test-coverage-check`, so a coverage failure is the local gate's
failure and reproduces locally.

Note that on Dependabot and fork runs `SONAR_TOKEN` and `SNYK_TOKEN` are empty,
so both jobs skip and report green. A green Sonar or Snyk check on a Dependabot
PR means nothing was scanned. They run for real on this branch, because it is
pushed from the repo rather than by Dependabot.

### 5. Changelog and commit

**A security pass must leave a `### Security` entry under `## [Unreleased]`.** This
is the one thing that cannot be skipped. `scripts/make-tag` refuses to cut a
release when `[Unreleased]` is empty, so with no entry there is no way to ship the
patched build at all, and the fix ends up riding along with whatever feature lands
next. The entry is also the only way a user learns why the patch release is worth
taking, since by definition they cannot see the change.

This is an explicit carve-out from the usual "user-visible changes only" rule; see
the Changelog section in `DESIGN.md`. Write the advisory IDs and what they affect,
not just "bumped dependencies". Routine bumps carrying no security fix do not each
need a line, so summarize those under `### Changed`.

Do not touch version numbers or tags. Releases go through `scripts/make-tag`.
Never reference private IKE-standards overlays in this repo's commits or PR text.

Stage every file you touched. `make lint` and `make test-coverage-check` pass on
working-tree state, but CI only sees what is committed:

```bash
git status
make check     # must be green on the staged tree
```

### 6. Push and verify

```bash
git push -u origin chore/security-upgrades-<YYYY-MM>
gh pr create --fill
```

Then **watch the checks actually go green**. This branch exists specifically to
fix red gates, so a green local run is not the deliverable:

```bash
gh pr checks --watch
```

Merging this PR auto-closes the Dependabot PRs whose commits it contains.

`main` is branch-protected with `test`, `lint`, `build`, `snyk` and `govulncheck`
required and strict/up-to-date enforced, so all five have to be green and the
branch has to be current with `main` before it will merge.

Known transient: `make setup-envtest` downloads envtest binaries from a GitHub
release on every test and sonar run and intermittently 504s. It hit the
`sonarcloud` job on the 2026-09 pass while `test` downloaded the same tarball
fine, which is the signature of a flake rather than a break. Clear it with:

```bash
gh run rerun <run_id> --failed
```

Verify a scanner actually scanned before calling it green. Both of these skip
silently when their token is absent, and a skipped job reports pass:

```bash
gh run view --job <job_id> --log | grep -F "Run Snyk" | grep -iE "Tested .* dependencies|issues found"
gh run view --job <job_id> --log | grep -F "SonarCloud Scan" | grep -iE "ANALYSIS SUCCESSFUL|EXECUTION"
```

A real Snyk run prints `Tested N dependencies for known issues`; a real Sonar run
prints `ANALYSIS SUCCESSFUL` and `EXECUTION SUCCESS`.

## Checklist

- [ ] Every open Dependabot PR either cherry-picked onto the branch or explicitly noted as superseded
- [ ] `git rev-list --merges origin/main..HEAD` is empty, so "Rebase and merge" works
- [ ] `go.mod` and `go.sum` conflicts resolved to the higher version on both sides
- [ ] `go` directive at the latest patch of its minor line; `govulncheck` clean
- [ ] Each red gate diagnosed from its real log, not assumed to be a finding
- [ ] `make check` green on the committed tree
- [ ] `CHANGELOG.md` `[Unreleased]` has a `### Security` entry naming the advisories, so a patch release can be cut
- [ ] PR checks watched to completion
