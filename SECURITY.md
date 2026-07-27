# Security Policy

## Supported versions

| Version | Supported |
| ------- | --------- |
| Latest  | Yes       |
| Older   | No        |

We support only the current release. Please upgrade before reporting a vulnerability.

## Reporting a vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Report vulnerabilities privately via GitHub's
[Security Advisories](https://github.com/tight-line/ballast/security/advisories/new)
feature (Settings > Security > Advisories > New draft advisory).

Please include:

- A description of the vulnerability and its potential impact
- Steps to reproduce or a proof-of-concept (if safe to share)
- The version(s) affected
- Any suggested mitigations you are aware of

We aim to acknowledge reports within 3 business days and to provide a resolution
timeline within 10 business days.

## Disclosure policy

Once a fix is available we will:

1. Release a patched version
2. Publish a GitHub Security Advisory crediting the reporter (unless anonymity is requested)
3. Add an entry to [CHANGELOG.md](CHANGELOG.md)

## Security posture and supply-chain hardening

`ballastd` runs inside the cluster with permission to watch and mutate workloads,
so a compromised build or dependency would be a high-value foothold. We treat the
software supply chain as the primary attack surface and defend it in layers:
dependencies, the build pipeline, published artifacts, and the runtime. This
section is a living checklist of what is in place today and what is planned. It is
intentionally light on exploitable specifics; report anything sensitive privately
via the process above.

### Dependencies

- [x] `govulncheck` gate in CI (symbol-level, tokenless) runs on every push and
      pull request, including Dependabot and fork PRs that cannot access secrets
- [x] Snyk (high-severity threshold) and SonarCloud scanning on trusted runs, on
      `main`, and on a weekly schedule
- [x] Dependabot version updates across every ecosystem the repo ships from
      (`gomod`, `github-actions`, `docker`, `devcontainers`)
- [x] Dependabot alerts and automated security-update PRs enabled
- [x] `dependency-review-action` on pull requests blocks newly introduced
      vulnerable or license-incompatible dependencies before merge
- [x] CodeQL static analysis (SAST) on pushes, pull requests, and a weekly schedule

### Build pipeline

- [x] Every GitHub Action pinned to a full commit SHA (no mutable tags)
- [x] Docker base images pinned by digest (`golang` builder, `distroless` runtime)
- [x] Reproducible build flags (`-trimpath`, pinned `-ldflags`); Go toolchain
      single-sourced from `go.mod` via `go-version-file`
- [x] Secret-dependent CI steps fail safe: they skip rather than run on untrusted
      (Dependabot/fork) runs, so repository secrets are never exposed to builds of
      untrusted dependency code
- [x] Untrusted (fork) pull requests cannot reach registry credentials: the PR
      image build is restricted to same-repo pull requests
- [x] Least-privilege `permissions:` on every workflow (read-only by default;
      write scopes granted only to the jobs that need them)
- [x] Egress control on CI runners via `step-security/harden-runner`: block-mode
      enforcement with observed-traffic allowlists on every workflow
- [x] OpenSSF Scorecard workflow

### Published artifacts

- [x] Keyless (cosign/OIDC) signature for the released **container image**
- [ ] Keyless signature for the **Helm chart** (in progress)
- [x] SLSA build-provenance attestation for the released image (OCI + GitHub)
- [x] Software Bill of Materials (SBOM) attached as an attestation

Verify a released image (all keyless, no public key needed):

```bash
IMAGE=ghcr.io/tight-line/ballast:v0.4.8   # use the tag you pulled
IDENTITY='^https://github\.com/Tight-Line/ballast/\.github/workflows/release\.yml@refs/tags/v'
ISSUER=https://token.actions.githubusercontent.com

cosign verify "$IMAGE" \
  --certificate-identity-regexp "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER"

cosign verify-attestation "$IMAGE" --type spdxjson \
  --certificate-identity-regexp "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER"
```

### Runtime

- [x] Distroless, non-root runtime image (`USER 65532`, no shell)
- [ ] Least-privilege review of the operator's RBAC (scoped to the verbs and
      resources it actually needs)
- [ ] Explicit hardened pod `securityContext` in the Helm chart
      (`readOnlyRootFilesystem`, drop all capabilities, `seccompProfile: RuntimeDefault`)
- [ ] Deployment guidance for admission-time verification (policy-controller or
      Kyverno) so clusters admit only signed, provenanced images

### Repository controls

- [x] `main` protected: pull request required, status checks
      (`test`, `lint`, `build`, `snyk`, `govulncheck`) must pass and be up to date,
      force-pushes and deletions blocked
- [x] Secret scanning with push protection enabled
- [x] Pre-commit hooks (`gitleaks` secret scan, `golangci-lint`, `shellcheck`,
      end-of-file and trailing-whitespace fixers)
