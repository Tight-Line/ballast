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
- [ ] `dependency-review-action` on pull requests to block newly introduced
      vulnerable or license-incompatible dependencies before merge

### Build pipeline

- [x] Every GitHub Action pinned to a full commit SHA (no mutable tags)
- [x] Docker base images pinned by digest (`golang` builder, `distroless` runtime)
- [x] Reproducible build flags (`-trimpath`, pinned `-ldflags`); Go toolchain
      single-sourced from `go.mod` via `go-version-file`
- [x] Secret-dependent CI steps fail safe: they skip rather than run on untrusted
      (Dependabot/fork) runs, so repository secrets are never exposed to builds of
      untrusted dependency code
- [ ] Least-privilege `permissions:` blocks on every workflow
- [ ] Egress monitoring on CI runners (e.g. `step-security/harden-runner`)
- [ ] OpenSSF Scorecard workflow and badge

### Published artifacts

- [ ] Keyless (cosign/OIDC) signatures for the container image and Helm chart
- [ ] SLSA build-provenance attestation for released images
- [ ] Software Bill of Materials (SBOM) generated and attached as an attestation

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
