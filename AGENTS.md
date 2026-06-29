# ballast — Agent Guide

> This file is the primary entry point for AI agents working on ballast.
> Read DESIGN.md for full system context before making changes.
> Read IMPLEMENTATION_PLAN.md to find the current phase and key files.

## Project Overview

Ballast is a Kubernetes operator that automatically right-sizes workload resource requests and limits based on real operational history. Workloads opt in via pod template annotations; Ballast never touches a workload without explicit `ballast.tightlinesoftware.com/measure: "true"`.

The operator has three main behaviors:
1. **Measure** — collect per-container CPU/memory usage samples into Redis time-series keys
2. **Apply** — patch resource requests/limits at admission time when a pod is admitted
3. **Resize** — adjust resources on running pods via the Kubernetes in-place resize API (1.35+)

Nothing is applied or resized until the matching `WorkloadProfile` has accumulated enough history and its `meetsThreshold` field is true. `autoresize` is simply shorthand for `measure + apply + resize` as a single annotation.

## Quick Start

```bash
make check        # Full gate: lint + coverage + build
make test         # Run tests
make build        # Build bin/ballastd
make fmt          # Format code
make lint-fix     # Auto-fix lint issues
make manifests    # Regenerate CRDs and RBAC from markers
make generate     # Regenerate DeepCopy methods
```

## Key Files

| File | Purpose |
|---|---|
| `cmd/ballastd/main.go` | Entry point; all CLI flags; creates kubebuilder manager; registers kill switch, all controllers, and webhook |
| `api/v1/ballastconfig_types.go` | `BallastConfig` CRD — cluster-scoped singleton; `identityLabels`, `orphanTTL`, `retentionWindow`, `suspended` |
| `api/v1/metricssource_types.go` | `MetricsSource` CRD — names a plugin type (`spec.type`) and its poll config |
| `api/v1/clusterresourcepolicy_types.go` | `ClusterResourcePolicy` CRD — selector, metrics slice, readiness config, behaviors |
| `api/v1/resourcepolicy_types.go` | `ResourcePolicy` CRD — same spec as `ClusterResourcePolicy`; namespace-scoped; always beats `ClusterResourcePolicy` |
| `api/v1/workloadprofile_types.go` | `WorkloadProfile` CRD — status-only; `tupleLabels`, `containers` (usageStats + recommendations), `meetsThreshold`, `activeWorkloads`, conditions |
| `internal/killswitch/killswitch.go` | `KillSwitch` reconciler; `IsActive()`/`Reason()` hot path; watches ConfigMap `ballast-kill-switch` and `BallastConfig.spec.suspended` |
| `internal/logger/logger.go` | `New(component, level, format) logr.Logger` backed by zap; `newWithWriter` is the testable variant |
| `internal/policy/resolver.go` | `Resolver`; `Resolve(ctx, Input) (*ResolvedPolicy, error)` — evaluates namespace/annotation/label selectors; `ResourcePolicy` beats `ClusterResourcePolicy` regardless of priority |
| `internal/store/client.go` | `Client` interface (go-redis subset); `NewClient(redisURL)` |
| `internal/store/keys.go` | `TupleHash(labels)`, `MetricKey(hash, container, resource)`, `AllKeysForHash(ctx, client, hash)` |
| `internal/store/metrics.go` | `AddSample`, `QueryWindow`, `ExpireOlderThan`, `SampleCount`, `TimeRange`, `DeleteKey`, `EnforceReservoirCap` |
| `internal/store/percentiles.go` | `ComputeStats([]int64) Stats` — p50/p95/p99/max/mean/stddev/CV |
| `internal/plugin/plugin.go` | `MetricsPlugin` interface; `WorkloadIdentity`, `TimeWindow`, `ContainerStats` types |
| `internal/plugin/registry.go` | `Register(p)`, `Get(typeName)` — global plugin registry; plugins self-register via `init()` |
| `internal/plugin/kubernetes/plugin.go` | `kubernetesMetrics` plugin — calls in-cluster metrics API; token-bucket rate limiting; exponential backoff on errors |
| `internal/stats/aggregator.go` | `EvaluateReadiness(Stats, firstMs, lastMs, ReadinessConfig) bool`; `ComputeRecommendation(Stats, MetricConfig) (resource.Quantity, error)` |
| `internal/validation/annotations.go` | `ValidateAnnotations(map[string]string) error` — enforces annotation combination rules |
| `internal/controller/workloadwatcher/controller.go` | Watches pods; creates/updates `WorkloadProfile`; `ProfileName(tupleLabels)` and `ExtractTupleLabels(podLabels, identityLabels)` exported for webhook use |
| `internal/controller/metricscollector/controller.go` | Reconciles `WorkloadProfile` on timer; polls plugins; writes to Redis; updates status with stats and recommendations |
| `internal/controller/resourceadjuster/controller.go` | Watches `WorkloadProfile` status changes; detects drift; issues in-place pod resize patches; exports `ExceedsDrift`, `CapChange`, `ResolveFieldThreshold`, `ParseResizeInterval` |
| `internal/webhook/pod_mutator.go` | `PodMutator` admission handler; `Handle`, `resolveApplyProfile`, `mutate`, `applyRecommendations`; registered at `/mutate-v1-pod` |
| `charts/ballast/values.yaml` | All Helm configurable settings with defaults |
| `charts/ballast/templates/deployment.yaml` | Operator deployment; mounts cert Secret; passes all CLI flags from values |
| `charts/ballast/templates/mutatingwebhookconfiguration.yaml` | Webhook registration; `failurePolicy: Fail`; cert-manager `caBundle` injection annotation |
| `charts/ballast/templates/ballastconfig.yaml` | Creates the `BallastConfig` singleton from Helm values |

## Architecture

```
Pod CREATE ──► PodMutator (admission webhook)
                  │  validates annotations
                  │  applies recommendations if profile ready (meetsThreshold)
                  │
                  ▼
           WorkloadWatcher (controller, pod events)
                  │  creates WorkloadProfile on first pod seen
                  │  stamps ballast.tightlinesoftware.com/profile-ref on pod
                  │  increments/decrements activeWorkloads
                  │  triggers orphan TTL cleanup
                  │
                  ▼
           MetricsCollector (controller, timer)
                  │  polls kubernetesMetrics plugin
                  │  writes samples to Redis sorted sets
                  │  enforces retention window + reservoir cap
                  │  computes p50/p95/p99/CV; evaluates readiness
                  │  updates WorkloadProfile status with recommendations
                  │
                  ▼
           ResourceAdjuster (controller, WorkloadProfile events)
                  │  detects drift between current and recommended values
                  │  caps adjustment per cycle (maxChangePerCycle)
                  └──► in-place pod resize (Kubernetes 1.35+)
```

KillSwitch is a side controller that caches whether the emergency ConfigMap exists or `BallastConfig.spec.suspended` is true. All controllers call `ks.IsActive()` before taking any external action and log at `warn` with `kill_switch: true` when suppressed.

## CRD Types

All types are in `api/v1/`. Auto-generated files (`zz_generated.*.go`) must never be edited directly — regenerate with `make generate` (deepcopy) or `make manifests` (CRDs, RBAC).

| Kind | Scope | Purpose |
|---|---|---|
| `BallastConfig` | Cluster | Singleton config: `identityLabels`, `orphanTTL`, `retentionWindow`, `suspended` |
| `MetricsSource` | Cluster | Links a plugin type name (`spec.type`) to poll config (`pollInterval`, `reservoirSize`) |
| `ClusterResourcePolicy` | Cluster | Selector (kinds, namespaces, annotations, labelSelector) + metrics slice + readiness + behaviors |
| `ResourcePolicy` | Namespace | Same spec as `ClusterResourcePolicy`; namespace-scoped; overrides any `ClusterResourcePolicy` |
| `WorkloadProfile` | Cluster | Status-only; holds usage stats, recommendations, `meetsThreshold`, `activeWorkloads`, and conditions |

`WorkloadProfile` names are derived by sorting identity label values and joining them with `--`, e.g., labels `{app: billing, component: api}` → `billing--api`. The `ProfileName(tupleLabels)` function in `internal/controller/workloadwatcher/controller.go` is authoritative.

## Controllers

### KillSwitch (`internal/killswitch/`)

Not a workload reconciler — it watches the `ballast-kill-switch` ConfigMap (in the operator namespace) and the `BallastConfig` singleton, then caches the result in an in-memory `sync.RWMutex`. `IsActive()` and `Reason()` acquire only a read lock. Registered via `ks.SetupWithManager(mgr)`.

### WorkloadWatcher (`internal/controller/workloadwatcher/`)

Two reconcilers share the package:

- `PodReconciler` — watches pods carrying any Ballast behavior annotation. On CREATE: reads `BallastConfig.identityLabels`, extracts the identity tuple, creates `WorkloadProfile` if absent, increments `activeWorkloads`, stamps `profile-ref` annotation on the pod. On DELETE: decrements `activeWorkloads`; sets `Orphaned` condition when it reaches zero. Kill switch suppresses CREATE path only; DELETE path always runs so accounting stays correct.
- `ProfileReconciler` — watches `WorkloadProfile` objects. Checks orphan TTL; if exceeded, purges Redis keys via `AllKeysForHash`/`DeleteKey` and deletes the profile.

Exported helpers used by the webhook: `ExtractTupleLabels(podLabels, identityLabels)` and `ProfileName(tupleLabels)`.

### MetricsCollector (`internal/controller/metricscollector/`)

Reconciles `WorkloadProfile` objects. On each cycle:
1. Resolves matched policy via `policy.Resolver`; loads `MetricsSource` from policy
2. Looks up plugin from the global registry by source type
3. Calls `plugin.FetchStats(ctx, identity, window)`
4. Writes samples to Redis; enforces retention window and reservoir cap
5. Queries Redis for the full retention window; computes stats via `store.ComputeStats`
6. Evaluates readiness via `stats.EvaluateReadiness`
7. If ready, computes recommendations via `stats.ComputeRecommendation`
8. Updates `WorkloadProfile` status

Dry-run (`--dry-run-measure`) skips steps 4 and 8. Kill switch skips both.

### ResourceAdjuster (`internal/controller/resourceadjuster/`)

Triggered by `WorkloadProfile` status changes and a periodic requeue timer (`behaviors.resize.interval`, default 15 minutes). For each pod with `resize` (or `autoresize`) annotation and a ready profile:
1. Calls `ExceedsDrift(current, recommended, thresholdPct)` per container/resource/field
2. If drift exceeded, calls `CapChange(current, recommended, maxChangePct)` to bound the adjustment
3. Issues the resize patch via the pod resize subresource
4. On failure (node pressure, infeasible): emits a Kubernetes Event and stamps `resize-blocked` annotation; retries on next cycle

Threshold lookup follows a coalesce chain: per-resource field override → resize default → global default (20%). Dry-run (`--dry-run-resize`) logs the resize without patching. Kill switch suppresses all action.

## Admission Webhook

`internal/webhook/pod_mutator.go` — registered at `/mutate-v1-pod` for pod CREATE.

Flow:
1. Kill switch active → allow without mutation; log `warn`
2. `ValidateAnnotations` fails → deny with descriptive message
3. Profile not ready (`meetsThreshold` false) → allow without mutation
4. `apply` or `autoresize` annotation present + profile ready → patch container `resources.requests` and `resources.limits` per profile recommendations; stamp `policy-ref` annotation
5. Dry-run (`--dry-run-apply`) → log the patch, admit without mutation

TLS: the Helm chart creates a cert-manager `Issuer` + `Certificate` (self-signed). cert-manager injects the `caBundle` into `MutatingWebhookConfiguration` automatically.

## Plugin Interface

`internal/plugin/plugin.go` defines the interface all metrics plugins must implement:

```go
type MetricsPlugin interface {
    Type() string
    FetchStats(ctx context.Context, id WorkloadIdentity, window TimeWindow) ([]ContainerStats, error)
}
```

The global registry in `internal/plugin/registry.go` maps type names to plugin instances. Plugins self-register from `init()`.

### Adding a new plugin

1. Create `internal/plugin/<name>/plugin.go` implementing `MetricsPlugin`.
2. Call `plugin.Register(&MyPlugin{})` in the package `init()` function.
3. Add a blank import of your plugin package in `cmd/ballastd/main.go`.
4. Create a `MetricsSource` object in the cluster with `spec.type` matching your `Type()` return value.

The built-in `kubernetesMetrics` plugin calls the in-cluster metrics API. It ignores the `TimeWindow` parameter and returns current point-in-time measurements. External plugins (e.g., Prometheus) should use the window for historical range queries.

The `kubernetesMetrics` plugin uses a token-bucket rate limiter (configurable RPS) and exponential backoff (base 1s, configurable max, default ceiling 5 minutes) on API errors. New plugins should implement similar defensive patterns.

## Redis Data Model

Keys follow the pattern `ballast:metrics:{tupleHash}:{container}:{resource}`.

- `tupleHash` — 16-character lowercase hex prefix of SHA-256 of sorted `key=value\n` pairs from the identity tuple (see `store.TupleHash`)
- `container` — container name as it appears in the pod spec
- `resource` — `cpu`, `memory`, or `ephemeral-storage`

Each key is a Redis sorted set. Score = Unix timestamp in milliseconds. Member = string-encoded value (millicores for CPU, bytes for memory/ephemeral-storage).

Key helpers in `internal/store/`:
- `TupleHash(labels map[string]string) string` — deterministic hash
- `MetricKey(tupleHash, container, resource string) string` — full key
- `AllKeysForHash(ctx, client, tupleHash) ([]string, error)` — scans with `SCAN` + prefix match for orphan cleanup

## Testing Strategy

- **Controller tests** — use `sigs.k8s.io/controller-runtime/pkg/envtest` (embedded etcd + API server). Tests live alongside their controller as `*_test.go`.
- **Redis store tests** — use `github.com/alicebob/miniredis/v2` as a drop-in `Client`. No real Redis required.
- **Policy and validation tests** — pure unit tests using a fake controller-runtime client.
- **Webhook tests** — envtest with a self-signed cert generated in `TestMain`; covers the full admission flow.

Coverage gate: `make test-coverage-check` enforces 100% coverage. Lines that genuinely cannot be tested (live Redis required, transient API errors, `json.Marshal` on well-typed structs) use `// coverage:ignore - <reason>`. Do not reach for this comment without first writing the test — it defeats the gate.

Run the full gate: `make check` (lint + coverage + build).

## Build and CI

| Target | What it does |
|---|---|
| `make check` | Full gate: golangci-lint + 100% coverage check + binary build |
| `make test` | Run tests with envtest |
| `make test-coverage` | Run tests, produce `coverage.out` |
| `make test-coverage-check` | Enforce 100% coverage via `scripts/check-coverage.sh` |
| `make build` | Build `bin/ballastd` |
| `make lint` | Run golangci-lint |
| `make lint-fix` | Auto-fix lint issues |
| `make fmt` | Run goimports |
| `make manifests` | Regenerate CRD YAML, RBAC, and webhook manifests from kubebuilder markers |
| `make generate` | Regenerate DeepCopy methods |
| `make tools` | Install goimports |
| `make setup-hooks` | Install pre-commit hook (`scripts/pre-commit`) |

### GitHub Actions

| Workflow | Triggers | What it does |
|---|---|---|
| `ci.yml` | PR + main push | Parallel test / lint / build; uploads `coverage.filtered.out` to Codecov |
| `pr-images.yml` | PR push | Builds `ghcr.io/tight-line/ballast:pr-<n>-<sha>` and comments on the PR |
| `release.yml` | Tag push (`v*`) | Builds `ghcr.io/tight-line/ballast:<tag>` + `:latest`; packages and publishes `ballast-<ver>.tgz` to the Helm repo via chart-releaser |
| `snyk.yml` | PR + main + weekly | Dependency vulnerability scan (high+ severity) |
| `sonar.yml` | PR + main | SonarCloud static analysis |

### Release workflow

Run `scripts/make-tag <version>` (e.g. `scripts/make-tag 0.1.0`) to cut a release. The script:
1. Runs `make check`
2. Bumps `charts/ballast/Chart.yaml` version and `appVersion`
3. Moves the `[Unreleased]` CHANGELOG section to `[<version>] - <date>`
4. Commits and tags `v<version>`

Then `git push origin main v<version>` triggers `release.yml`, which:
- Builds and pushes `ghcr.io/tight-line/ballast:v<version>` and `:latest` (multi-arch: amd64 + arm64)
- Runs `helm dependency update` then `helm/chart-releaser-action` to package `ballast-<version>.tgz`, upload it as a GitHub Release asset, and update `index.yaml` on the `gh-pages` branch

The Helm repo (`https://tight-line.github.io/ballast`) is served from the `gh-pages` branch; GitHub Pages must be enabled on the repository pointing to that branch.

### PR image workflow

On every PR push `pr-images.yml` builds and pushes:

```
ghcr.io/tight-line/ballast:pr-<PR_NUMBER>-<SHORT_SHA>
```

The workflow comments the tag on the PR with a ready-to-paste Helm override. Images expire approximately 15 days after the PR closes.

```yaml
# Helm values override to use a PR image
image:
  repository: ghcr.io/tight-line/ballast
  tag: "pr-42-abc1234"
```

## Coding Standards

- **Coverage first.** Write a test before reaching for `// coverage:ignore - <reason>`. Use the ignore comment only when testing is genuinely impossible (live Redis, transient API errors, `json.Marshal` on a struct that cannot fail).
- **Logging.** Use `ctrl.LoggerFrom(ctx)` inside reconcile loops. Use `logger.New(component, level, format)` at startup. Suppressed-by-kill-switch actions log at `warn` with `kill_switch: true`. Dry-run actions log at `info` with `dry_run: true`.
- **No direct API calls from controllers.** Read from the controller-runtime cache. Only the `store` and `plugin` packages call external services directly.
- **Annotation validation.** Call `validation.ValidateAnnotations` before acting on any annotation combination.
- **Imports.** `goimports` with local prefix `github.com/tight-line/ballast` (see `.golangci.yml`). Run `make fmt` before committing.

## Important: Never Edit These (Auto-Generated)

- `config/crd/bases/*.yaml` — from `make manifests`
- `config/rbac/role.yaml` — from `make manifests`
- `api/*/zz_generated.*.go` — from `make generate`
- `PROJECT` — kubebuilder metadata

## Important: Never Remove Scaffold Markers

Do NOT delete `// +kubebuilder:scaffold:*` comments. The kubebuilder CLI injects code at these markers.
