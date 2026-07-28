# Admission-time image verification (opt-in)

Ballast's release container image is keyless-signed with cosign (GitHub OIDC +
the public sigstore infrastructure), and carries SLSA build-provenance and an
SBOM as attestations. You can verify a pulled image by hand with `cosign`
(see the "Verifying the release" section of the top-level [README](../../README.md)
and [SECURITY.md](../../SECURITY.md)).

These examples go one step further: they make the **cluster** enforce that
verification at admission time, so an image that isn't validly signed by the
Ballast release workflow can't be scheduled at all.

## This is opt-in, on purpose

The Ballast Helm chart does **not** install any of this. Admission-time
verification is a cluster-operator decision, not a workload's:

- It requires an admission engine (Kyverno or policy-controller) to already be
  running in the cluster.
- Its blast radius is cluster-wide: a policy can block pods well beyond Ballast,
  and a misconfigured one can lock you out of your own cluster.
- Admission webhooks are cluster-critical infrastructure; a workload chart
  shouldn't quietly reinstall or reconfigure them.

So the chart stays out of it, and these manifests are here for you to review,
scope, and apply deliberately.

## Choose an engine

Both are free, open-source, and verify the same thing; pick whichever your
cluster already runs (or prefers).

| Engine | File | Notes |
|---|---|---|
| **Kyverno** | [`kyverno/verify-ballast-image-keyless.yaml`](kyverno/verify-ballast-image-keyless.yaml) | CNCF project, general-purpose policy engine. Most clusters that run any policy engine run this one. |
| **sigstore policy-controller** | [`policy-controller/ballast-keyless.yaml`](policy-controller/ballast-keyless.yaml) | Purpose-built for cosign/keyless verification; narrower scope. Requires per-namespace opt-in labels. |

Both examples pin the exact signing identity of the Ballast release workflow:

- **Subject** (regexp): `^https://github\.com/Tight-Line/ballast/\.github/workflows/release\.yml@refs/tags/v`
- **Issuer**: `https://token.actions.githubusercontent.com`
- **Image**: `ghcr.io/tight-line/ballast:*`

Both are fail-closed by default (block on a missing or mismatched signature).
Each file's header comments cover its prerequisites and how to extend it to also
require the SLSA provenance attestation.

## Roll out safely

1. Install your chosen engine and confirm its webhook is healthy.
2. Apply the policy to a **non-production** namespace first. For
   policy-controller, label that namespace:
   `kubectl label namespace <ns> policy.sigstore.dev/include=true`.
3. Confirm fail-closed behavior: try to run an **unsigned** image (or a
   deliberately wrong tag) and check that admission is rejected. Read the
   rejection message with `kubectl describe` on the blocked Pod/ReplicaSet.
4. Only then widen the policy to the namespaces where Ballast (and anything else
   you want gated) runs.
