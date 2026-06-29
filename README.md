# Ballast

[![Known Vulnerabilities](https://snyk.io/test/github/Tight-Line/ballast/badge.svg)](https://snyk.io/test/github/Tight-Line/ballast)
[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=Tight-Line_ballast&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=Tight-Line_ballast)
[![codecov](https://codecov.io/gh/Tight-Line/ballast/branch/main/graph/badge.svg)](https://codecov.io/gh/Tight-Line/ballast)

Ballast is a Kubernetes operator that automatically right-sizes workload resource requests and limits based on real operational history. It is a more active alternative to [Fairwinds Goldilocks](https://github.com/FairwindsOps/goldilocks): rather than suggesting changes, it applies them — at admission time and on running pods via in-place resize (Kubernetes 1.35+).

## How it works

Workloads opt in with annotations on their pod templates. Ballast observes real CPU and memory utilization, accumulates a rolling history keyed to a *workload identity tuple* (a set of pod labels you configure), and uses that history to:

1. **Measure** — collect per-container usage samples into a time-series store (Redis/Valkey).
2. **Apply** — patch resource requests and limits at admission time when a pod is created.
3. **Resize** — adjust resources on running pods via the Kubernetes in-place resize API (1.35+).

`autoresize` is shorthand for all three: it enables measure, apply, and resize with a single annotation. Nothing is applied or resized until the `WorkloadProfile` meets its readiness threshold — that is normal behavior for any resize-enabled workload.

Pod eviction for cluster rebalancing is handled by [Kubernetes Descheduler](https://github.com/kubernetes-sigs/descheduler) — see the Annotation Contract section for details.

## Implementation Status

| Phase | What | Status |
|---|---|---|
| 1 | Repository scaffold, kubebuilder init, CI/CD | Complete |
| 2 | CRD type definitions | Complete |
| 3 | Logger infrastructure and kill switch | Complete |
| 4 | Policy resolution | Complete |
| 5 | Redis/Valkey client layer | Complete |
| 6 | Plugin interface and `kubernetesMetrics` plugin | Complete |
| 7 | WorkloadWatcher controller | Complete |
| 8 | MetricsCollector controller | Complete |
| 9 | Admission webhook | Complete |
| 10 | ResourceAdjuster controller | Complete |
| 11 | Helm chart | Complete |
| 12 | Polish and release readiness | Complete |

## Prerequisites

- Kubernetes 1.35+ (required for in-place pod resize; earlier versions support measure and apply but not resize)
- [metrics-server](https://github.com/kubernetes-sigs/metrics-server) installed in the cluster
- TLS certificate for the admission webhook (see [Webhook TLS](#webhook-tls) below)
- A Redis-compatible store (Ballast ships with a bundled Valkey via Helm; an existing Redis or Valkey instance works too)

## Installation

cert-manager must already be installed in the cluster (Ballast does not install it):

```bash
helm repo add ballast https://tight-line.github.io/ballast
helm repo update

helm install ballast ballast/ballast \
  --namespace ballast-system \
  --create-namespace
```

The chart ships with a sensible default for `ballastConfig.identityLabels` (`app.kubernetes.io/name` + `app.kubernetes.io/component`). Read the section below before overriding it — the choice has cluster-wide consequences.

## WorkloadProfile Identity

Ballast groups pods into `WorkloadProfile` objects by matching a configurable set of pod label keys called the **identity tuple**. The tuple is defined once in `BallastConfig.spec.identityLabels` and applies to every namespace in the cluster.

**WorkloadProfiles are cluster-scoped.** Every pod in every namespace that shares the same label values for the identity keys feeds measurements into the same profile. This is intentional: forty dev namespaces all running the same billing app produce one well-sampled `WorkloadProfile`, not forty thin ones.

### Default: `name` + `component`

```yaml
ballastConfig:
  identityLabels:
    - app.kubernetes.io/name
    - app.kubernetes.io/component
```

This works well for clusters that run a single environment class. The frontend and backend of the same app get separate profiles; forty developers' copies of billing all contribute to the same `(billing, api)` profile.

### Mixed environments in the same cluster

If your cluster runs dev, staging, and production side-by-side and you want separate profiles per environment, add `ballast.tightlinesoftware.com/profile` to the identity tuple and apply it to your pods:

```yaml
# BallastConfig / Helm values
ballastConfig:
  identityLabels:
    - app.kubernetes.io/name
    - app.kubernetes.io/component
    - ballast.tightlinesoftware.com/profile
```

```yaml
# Pod template labels
labels:
  app.kubernetes.io/name: billing
  app.kubernetes.io/component: api
  ballast.tightlinesoftware.com/profile: prod
```

Now `(billing, api, prod)` and `(billing, api, dev)` are measured independently. Pods without the `ballast.tightlinesoftware.com/profile` label get a placeholder value in the profile name (`noprofile`) rather than being skipped, so opted-in pods always produce a profile.

> **Changing `identityLabels` wipes your operational history.** It redefines what constitutes a workload identity, so all existing `WorkloadProfile` objects are renamed and their accumulated Redis history is orphaned. Ballast starts fresh from zero samples. Plan your tuple before enrolling workloads.

## Annotation Contract

Add these annotations to your pod template specs to enroll workloads. Ballast never acts on a workload without explicit opt-in.

| Annotation | Meaning |
|---|---|
| `ballast.tightlinesoftware.com/measure: "true"` | Collect metrics; required for any other behavior |
| `ballast.tightlinesoftware.com/apply: "true"` | Patch requests/limits at admission time; requires `measure` |
| `ballast.tightlinesoftware.com/resize: "true"` | Adjust resources on running pods via in-place resize; requires `apply` |
| `ballast.tightlinesoftware.com/autoresize: "true"` | Shorthand for `measure` + `apply` + `resize`; mutually exclusive with `apply` and `resize` |

**Pod eviction** is deliberately out of scope for Ballast. Ballast keeps resource requests and limits accurate; cluster rebalancing based on those corrected values is best handled by [Kubernetes Descheduler](https://github.com/kubernetes-sigs/descheduler) (specifically its `LowNodeUtilization` strategy). This is a clean division of labor: Ballast gets the weight right, Descheduler decides where pods should sit.

**Example — full automation:**

```yaml
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: billing
        ballast.tightlinesoftware.com/profile: prod
      annotations:
        ballast.tightlinesoftware.com/autoresize: "true"
```

**Example — measure only (safe first step):**

```yaml
spec:
  template:
    metadata:
      annotations:
        ballast.tightlinesoftware.com/measure: "true"
```

## Verifying a WorkloadProfile

Once a pod with the `measure` annotation is running, Ballast creates a `WorkloadProfile` for its identity tuple. Check it with:

```bash
kubectl get workloadprofiles
kubectl describe workloadprofile billing--api--prod
```

The profile status shows accumulated usage statistics and recommendations once the readiness threshold is met (default: 500 samples collected over 24 hours):

```yaml
status:
  containers:
    - name: app
      usageStats:
        - resource: cpu
          samples: 1440
          p95: "240m"
          p99: "310m"
          cv: "0.46"
      recommendations:
        cpu:
          request: "288m"   # p95 * 1.2 headroom
          limit: "388m"     # p99 * 1.25 headroom
  meetsThreshold: true
  activeWorkloads: 3
```

## Kill Switch

Create a ConfigMap named `ballast-kill-switch` in the operator namespace to immediately halt all Ballast activity without a restart:

```bash
# Halt all Ballast activity
kubectl create configmap ballast-kill-switch -n ballast-system

# Resume
kubectl delete configmap ballast-kill-switch -n ballast-system
```

All suppressed actions are logged at `warn` level with `kill_switch: true`. Pod admission continues normally (webhook passes pods through without mutation).

For planned, GitOps-managed suspension, set `BallastConfig.spec.suspended: true` instead.

## Webhook TLS

Kubernetes requires the admission webhook server to present a TLS certificate trusted by the API server. Ballast supports three approaches, in order of preference:

**1. cert-manager (default)**

The Helm chart creates a self-signed `Issuer` and a `Certificate` resource. cert-manager provisions the cert, mounts it into the operator pod, and injects the CA bundle into the `MutatingWebhookConfiguration` automatically. This works on air-gapped clusters — no DNS or HTTP challenge, no external CA.

Requires [cert-manager](https://cert-manager.io) already installed in the cluster (Ballast uses it but does not install it). If cert-manager is already present — a common case — no extra setup is needed.

```yaml
# values.yaml (default)
certManager:
  enabled: true
```

**2. Kubernetes CertificateSigningRequest (future improvement)**

A Helm pre-install Job submits a CSR to the cluster's built-in CA. The resulting cert is written to a Secret that the operator mounts. No cert-manager dependency, but requires the Job's ServiceAccount to have `certificates.k8s.io/approve` permission — which some clusters restrict, requiring manual `kubectl certificate approve`.

Not yet implemented; tracked as a future Helm chart improvement.

**3. User-provided certificate (future improvement)**

Supply your own cert material (e.g. from an internal PKI or Vault) via Helm values. The chart skips cert-manager and CSR resources entirely and uses the provided Secret directly. The `caBundle` in the `MutatingWebhookConfiguration` must be set to the corresponding CA cert.

Not yet implemented; tracked as a future Helm chart improvement.

## Default MetricsSource and ClusterResourcePolicy

A fresh `helm install` ships two objects out of the box so measurements work without any extra setup:

### MetricsSource: `kubernetes-metrics`

```yaml
spec:
  type: kubernetesMetrics
  config:
    pollInterval: "60s"
    reservoirSize: 10000
```

This wires Ballast to the cluster's [metrics-server](https://github.com/kubernetes-sigs/metrics-server) (which must already be installed — it is not bundled). Samples are collected every 60 seconds and up to 10,000 samples per container per metric are retained in Redis.

To opt out and manage `MetricsSource` objects yourself:

```yaml
# values.yaml
defaultMetricsSource:
  enabled: false
```

### ClusterResourcePolicy: `default`

```yaml
spec:
  priority: 0
  metrics:
    - resource: cpu
      field: request
      source: kubernetes-metrics
      aggregation: p95
      headroom: "1.2"
    - resource: memory
      field: request
      source: kubernetes-metrics
      aggregation: p95
      headroom: "1.3"
  readiness:
    minDataPoints: 500
    minTimeSpan: "24h"
    maxCV: "1.5"
  behaviors:
    thresholds:
      default: "20%"
    resize:
      maxChangePerCycle: "50%"
      interval: "15m"
```

This catch-all policy applies to every opted-in pod in the cluster. Key design decisions:

- **Requests only, no limits.** Setting memory limits without careful profiling causes OOMKills; CPU limits cause throttling. Add limit entries in a higher-priority policy once you have enough history to trust the recommendations.
- **p95 with headroom.** CPU requests are set to the 95th-percentile usage × 1.2; memory at p95 × 1.3. Memory gets slightly more headroom because usage spikes are harder to absorb than CPU ones.
- **500 samples over 24 hours before acting.** This prevents premature resizes on workloads that haven't been observed long enough to produce stable recommendations. A high coefficient of variation (CV > 1.5) also blocks action — it means the workload is too spiky to size reliably.
- **20% drift threshold.** A resize only fires when the current resource value deviates from the recommendation by more than 20%, avoiding churn from minor fluctuations.
- **50% max change per cycle.** Caps how aggressively a single resize can move a value, giving workloads time to stabilize between adjustments.
- **Priority 0.** This is the lowest possible priority. Any `ClusterResourcePolicy` or `ResourcePolicy` with `priority > 0` wins for matched workloads, so you can override specific namespaces or workload kinds without touching this default.

To opt out and manage policies yourself:

```yaml
# values.yaml
defaultClusterResourcePolicy:
  enabled: false
```

To override just the readiness threshold or headroom for all workloads, patch the values directly:

```yaml
# values.yaml
defaultClusterResourcePolicy:
  readiness:
    minDataPoints: 200
    minTimeSpan: "6h"
  metrics:
    - resource: cpu
      field: request
      aggregation: p95
      headroom: "1.1"
    - resource: memory
      field: request
      aggregation: p95
      headroom: "1.2"
```

To add a tighter policy for production namespaces alongside the default, create a higher-priority `ClusterResourcePolicy`:

```yaml
apiVersion: ballast.tightlinesoftware.com/v1
kind: ClusterResourcePolicy
metadata:
  name: production
spec:
  priority: 10
  selector:
    namespaces:
      include: ["/.*-prod/", "/.*-production/"]
  metrics:
    - resource: cpu
      field: request
      source: kubernetes-metrics
      aggregation: p99
      headroom: "1.15"
    - resource: memory
      field: request
      source: kubernetes-metrics
      aggregation: p99
      headroom: "1.25"
  readiness:
    minDataPoints: 1000
    minTimeSpan: "72h"
    maxCV: "1.0"
```

## Dry-run Mode

Each action has an independent dry-run flag. They cascade: dry-running `measure` implies dry-running everything downstream.

| Flag | Helm value | Effect |
|---|---|---|
| `--dry-run-measure` | `dryRun.measure` | Compute stats, log what would be written; no Redis writes |
| `--dry-run-apply` | `dryRun.apply` | Log the patch; pod admitted without modification |
| `--dry-run-resize` | `dryRun.resize` | Log the resize; pod not touched |

All dry-run actions are logged at `info` level with `dry_run: true`.

## Development

```bash
# Prerequisites
make tools          # Install goimports
make setup-hooks    # Install pre-commit hook

# Common workflow
make check          # Full gate: lint + 100% coverage + build
make build          # Build bin/ballastd
make test           # Run tests with envtest
make lint-fix       # Auto-fix lint issues
make fmt            # Format code

# CRD / code generation (run after editing api/*_types.go)
make manifests      # Regenerate CRDs, RBAC, and webhook manifests
make generate       # Regenerate DeepCopy methods
```

The `make check` target is the gate for every PR and release. It runs golangci-lint, enforces 100% test coverage (uncovered lines require a `// coverage:ignore - <reason>` comment), and builds the binary.

### Local kind cluster

For iterating against a real cluster without pushing to GHCR, use the `helm-update-local` workflow. It builds a local image, loads it directly into kind (no registry push/pull), and installs the Helm chart.

**One-time setup**

```bash
# Create the kind cluster (any name works; pass it to every make command below)
kind create cluster --name ballast-dev

# Install cert-manager (required by the Ballast webhook)
helm repo add jetstack https://charts.jetstack.io --force-update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true

# Wait for cert-manager to be ready before deploying Ballast
kubectl rollout status deployment/cert-manager -n cert-manager
```

**Iterate: change code → rebuild → redeploy**

```bash
make helm-update-local KIND_CLUSTER=ballast-dev
```

This runs three steps in sequence:

1. **Build** — `docker build --platform linux/<host-arch>` tagged `:local`. The host architecture is detected automatically via `uname -m`, so the same command works on both ARM and x86 machines.
2. **Load** — `kind load docker-image` injects the image directly into the kind node; no registry push or GHCR credentials needed.
3. **Install** — `helm upgrade --install` deploys the chart into `ballast-system` with `image.pullPolicy=Never`, pinning it to the locally loaded image.

**Individual targets** (when you only need part of the cycle):

```bash
make docker-kind KIND_CLUSTER=ballast-dev      # Build + load image only
make helm-install-local                        # Install/upgrade chart only (uses last loaded image)
```

**Verify the deployment**

```bash
kubectl get pods -n ballast-system             # operator pod should be Running
kubectl logs -n ballast-system -l app.kubernetes.io/name=ballast -f
kubectl get ballastconfig                      # confirm CRD is installed
```

## Logging

Per-component log levels are configurable at startup:

```bash
ballastd \
  --log-level=info \
  --log-level-webhook=debug \
  --log-level-collector=warn \
  --log-format=text    # json (default) or text
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Copyright 2026 Tight Line Software LLC.

Licensed under the MIT License. See [LICENSE](LICENSE) for the full text.
