# Ballast

[![Known Vulnerabilities](https://snyk.io/test/github/Tight-Line/ballast/badge.svg)](https://snyk.io/test/github/Tight-Line/ballast)
[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=Tight-Line_ballast&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=Tight-Line_ballast)
[![codecov](https://codecov.io/gh/Tight-Line/ballast/branch/main/graph/badge.svg)](https://codecov.io/gh/Tight-Line/ballast)

Ballast is a Kubernetes operator that automatically right-sizes workload resource requests and limits based on real operational history. It is a more active alternative to [Fairwinds Goldilocks](https://github.com/FairwindsOps/goldilocks): rather than suggesting changes, it applies them — at admission time, on running pods via in-place resize (Kubernetes 1.35+), and via eviction when in-place adjustment is unavailable or insufficient.

## How it works

Workloads opt in with annotations on their pod templates. Ballast observes real CPU and memory utilization, accumulates a rolling history keyed to a *workload identity tuple* (a set of pod labels you configure), and uses that history to:

1. **Measure** — collect per-container usage samples into a time-series store (Redis/Valkey).
2. **Apply** — patch resource requests and limits at admission time when a pod is created.
3. **Resize** — adjust resources on running pods via the Kubernetes in-place resize API (1.35+).
4. **Evict** — evict pods to force re-admission with updated resources when resize is blocked.

Progressive modes (`autoresize`, `automagic`) start in measure-only mode and automatically activate adjustment once enough history has been collected.

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
| 9 | Admission webhook | Not started |
| 10 | ResourceAdjuster controller | Not started |
| 11 | Helm chart | Not started |
| 12 | Polish and release readiness | Not started |

## Prerequisites

- Kubernetes 1.35+ (required for in-place pod resize; earlier versions support measure/apply/evict but not resize)
- [metrics-server](https://github.com/kubernetes-sigs/metrics-server) installed in the cluster
- [cert-manager](https://cert-manager.io) installed (manages the admission webhook TLS certificate)
- A Redis-compatible store (Ballast ships with a bundled Valkey via Helm; an existing Redis or Valkey instance works too)

## Installation

> **Note:** The Helm chart is not yet available (Phase 11). These instructions will be updated when the chart ships.

Once the chart is available, installation will look like:

```bash
helm repo add tight-line https://tight-line.github.io/ballast
helm repo update

helm install ballast tight-line/ballast \
  --namespace ballast-system \
  --create-namespace \
  --set ballastConfig.identityLabels[0]=app.kubernetes.io/name \
  --set ballastConfig.identityLabels[1]=ballast.tightlinesoftware.com/profile
```

## Annotation Contract

Add these annotations to your pod template specs to enroll workloads. Ballast never acts on a workload without explicit opt-in.

| Annotation | Meaning |
|---|---|
| `ballast.tightlinesoftware.com/measure: "true"` | Collect metrics; required for any other behavior |
| `ballast.tightlinesoftware.com/apply: "true"` | Patch requests/limits at admission time; requires `measure` |
| `ballast.tightlinesoftware.com/resize: "true"` | Adjust resources on running pods via in-place resize; requires `apply` |
| `ballast.tightlinesoftware.com/evict: "true"` | Evict pods to force re-admission with updated resources; requires `apply` or `resize` |
| `ballast.tightlinesoftware.com/autoresize: "true"` | Progressive: measure-only until history threshold met, then `apply` + `resize` |
| `ballast.tightlinesoftware.com/automagic: "true"` | Progressive: measure-only until history threshold met, then `apply` + `resize` + `evict` |

**Example — full automation:**

```yaml
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: billing
        ballast.tightlinesoftware.com/profile: prod
      annotations:
        ballast.tightlinesoftware.com/automagic: "true"
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
kubectl describe workloadprofile billing--prod
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

## Dry-run Mode

Each action has an independent dry-run flag. They cascade: dry-running `measure` implies dry-running everything downstream.

| Flag | Helm value | Effect |
|---|---|---|
| `--dry-run-measure` | `dryRun.measure` | Compute stats, log what would be written; no Redis writes |
| `--dry-run-apply` | `dryRun.apply` | Log the patch; pod admitted without modification |
| `--dry-run-resize` | `dryRun.resize` | Log the resize; pod not touched |
| `--dry-run-evict` | `dryRun.evict` | Log which pod would be evicted; no eviction issued |

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
