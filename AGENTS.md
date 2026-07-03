# ballast — Agent Guide

> This file is the primary entry point for AI agents working on ballast.
> Read DESIGN.md for full system context before making changes.
> **For any change to enrollment or WorkloadProfile lifecycle, read
> [docs/convergence.md](docs/convergence.md) first** — it is the canonical
> convergence contract (sequence diagrams + invariants) and must be updated
> alongside such changes.
> The Key Files table below is the current reference. IMPLEMENTATION_PLAN.md is a
> historical record of the phased build-out, not a live status tracker.

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
| `internal/store/client.go` | `Client` interface (go-redis subset: RPush, LTrim, LRange, LLen, SetNX, Get, Del, Scan); `NewClient(redisURL)` |
| `internal/store/keys.go` | `TupleHash(labels)`, `MetricKey(hash, container, resource)`, `AllKeysForHash(ctx, client, hash)` |
| `internal/store/metrics.go` | `AddSample`, `QueryAll`, `FirstSeenMs`, `SampleCount`, `DeleteKey` — list-based API; see Redis Data Model |
| `internal/store/percentiles.go` | `ComputeStats([]int64) Stats` — p50/p95/p99/max/mean/stddev/CV |
| `internal/plugin/plugin.go` | `MetricsPlugin` interface; `WorkloadIdentity`, `TimeWindow`, `ContainerStats` types |
| `internal/plugin/registry.go` | `Register(p)`, `Get(typeName)` — global plugin registry; plugins self-register via `init()` |
| `internal/plugin/kubernetes/plugin.go` | `kubernetesMetrics` plugin — calls in-cluster metrics API; token-bucket rate limiting; exponential backoff on errors |
| `internal/stats/aggregator.go` | `EvaluateReadiness(Stats, firstMs, lastMs, ReadinessConfig) bool`; `ComputeRecommendation(Stats, MetricConfig) (resource.Quantity, error)` |
| `internal/validation/annotations.go` | `ValidateAnnotations(map[string]string) error` — enforces annotation combination rules |
| `internal/controller/workloadwatcher/controller.go` | Watches pods; creates/updates `WorkloadProfile`; `ProfileName(tupleLabels, identityLabels)` and `ExtractTupleLabels(podLabels, identityLabels)` exported for webhook use |
| `internal/controller/metricscollector/controller.go` | Reconciles `WorkloadProfile` on timer; polls plugins; writes to Redis lists; updates status with stats and recommendations |
| `internal/controller/resourceadjuster/controller.go` | Watches `WorkloadProfile` status changes; detects drift; issues in-place pod resize patches; exports `ExceedsDrift`, `CapChange`, `ResolveFieldThreshold`, `ParseResizeInterval` |
| `internal/webhook/pod_mutator.go` | `PodMutator` admission handler; `Handle`, `resolveApplyProfile`, `mutate`, `applyRecommendations`; registered at `/mutate-v1-pod` |
| `charts/ballast/values.yaml` | All Helm configurable settings with defaults |
| `charts/ballast/templates/deployment.yaml` | Operator deployment; mounts cert Secret; passes all CLI flags from values |
| `charts/ballast/templates/mutatingwebhookconfiguration.yaml` | Webhook registration; `failurePolicy: Fail`; cert-manager `caBundle` injection annotation |
| `charts/ballast/templates/ballastconfig.yaml` | Creates the `BallastConfig` singleton from Helm values |
| `charts/ballast/templates/metricssource.yaml` | Creates the default `kubernetes-metrics` MetricsSource (opt-out via `defaultMetricsSource.enabled: false`) |
| `charts/ballast/templates/clusterresourcepolicy.yaml` | Creates the default `default` ClusterResourcePolicy (opt-out via `defaultClusterResourcePolicy.enabled: false`) |

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
                  │  writes samples to Redis lists (RPUSH+LTRIM reservoir cap)
                  │  records first-seen timestamp per key (SET NX)
                  │  computes p50/p95/p99/CV; evaluates readiness
                  │  updates WorkloadProfile status with recommendations
                  │
                  ▼
           ResourceAdjuster (controller, WorkloadProfile events)
                  │  detects drift between current and recommended values
                  │  caps adjustment per cycle (maxChangePerCycle, % of gap)
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

`WorkloadProfile` names are derived by joining identity label values in `identityLabels` order with `--`, e.g., `identityLabels = ["app", "component"]` with labels `{app: billing, component: api}` → `billing--api`. The `ProfileName(tupleLabels, identityLabels)` function in `internal/controller/workloadwatcher/controller.go` is authoritative.

## Controllers

### KillSwitch (`internal/killswitch/`)

Not a workload reconciler — it watches the `ballast-kill-switch` ConfigMap (in the operator namespace) and the `BallastConfig` singleton, then caches the result in an in-memory `sync.RWMutex`. `IsActive()` and `Reason()` acquire only a read lock. Registered via `ks.SetupWithManager(mgr)`.

### WorkloadWatcher (`internal/controller/workloadwatcher/`)

> **Before changing enrollment or profile-lifecycle behavior, read
> [docs/convergence.md](docs/convergence.md)** — it holds the canonical sequence
> diagrams and design invariants for how convergence is achieved. Keep it in sync
> with any change here.

Enrollment is **level-triggered**: every pod CREATE/UPDATE reconcile recomputes the
desired profile from the pod's current labels and the current `identityLabels` and
reconciles toward it. The `profile-ref` stamp is a hint (a deterministic function of
identity), not the source of truth; it is trusted verbatim only on the DELETE path.

Two reconcilers share the package:

- `PodReconciler` — watches pods carrying any Ballast behavior annotation (also watches WorkloadProfile deletions and BallastConfig `identityLabels` changes to converge promptly). On CREATE/UPDATE: if enrolled, computes the target profile, creates the `WorkloadProfile` if absent (recreating it if deleted), stamps `profile-ref`, and recomputes `activeWorkloads` from live pods. If the computed name differs from the stamp → **migrate** (re-stamp, recount new and old). If no behavior annotation remains → **un-enroll** (remove `profile-ref` + finalizer, decrement old). On DELETE: reads the stamp (no recompute), recounts, sets `Orphaned` at zero. Kill switch suppresses the CREATE path only; DELETE/decrement always runs. Counts are recomputed from live pods (never `++/--`), so they self-heal.
- `ProfileReconciler` — watches `WorkloadProfile` objects. Owns the cleanup finalizer (`ballast.tightlinesoftware.com/profile-cleanup`): on any deletion it purges Redis keys via `AllKeysForHash`/`DeleteKey` before releasing the object, so manual `kubectl delete` clears history just like the orphan-TTL sweep. Orphan-TTL expiry issues the `Delete`; the finalizer does the purge.

Exported helpers used by the webhook: `ExtractTupleLabels(podLabels, identityLabels)` and `ProfileName(tupleLabels, identityLabels)`. `ProfileName` joins label values in `identityLabels` order (not alphabetically) so the name reads naturally, e.g., `nginx--nocomponent` when `identityLabels = ["name", "component"]`.

### MetricsCollector (`internal/controller/metricscollector/`)

Reconciles `WorkloadProfile` objects. On each cycle:
1. Guards on `status.selectorLabels` being set (written by WorkloadWatcher); requeues after 5s if nil to avoid collecting from all cluster pods during startup race
2. Resolves matched policy via `policy.Resolver`; loads `MetricsSource` from policy
3. Looks up plugin from the global registry by source type
4. Calls `plugin.FetchStats(ctx, identity, window)`
5. Appends samples to Redis lists via `store.AddSample` (RPUSH + LTRIM reservoir cap + SET NX first-seen timestamp)
6. Queries all samples via `store.QueryAll`; computes stats via `store.ComputeStats`
7. Gets first-seen timestamp via `store.FirstSeenMs` to evaluate time span coverage
8. Evaluates readiness via `stats.EvaluateReadiness`
9. If ready, computes recommendations via `stats.ComputeRecommendation`
10. Updates `WorkloadProfile` status

Dry-run (`--dry-run-measure`) skips steps 5 and 10. Kill switch skips both.

### ResourceAdjuster (`internal/controller/resourceadjuster/`)

Triggered by `WorkloadProfile` status changes and a periodic requeue timer (`behaviors.resize.interval`, default 15 minutes). For each pod with `resize` (or `autoresize`) annotation and a ready profile:
1. Calls `ExceedsDrift(current, recommended, thresholdPct)` per container/resource/field
2. If drift exceeded, calls `CapChange(current, recommended, maxChangePct, thresholdPct)` to bound the adjustment to `maxChangePct`% of the current→recommended gap; a capped step landing within the drift threshold applies the recommendation exactly (no Zeno tail)
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

Metric keys follow the pattern `ballast:metrics:{tupleHash}:{container}:{resource}`.

- `tupleHash` — 16-character lowercase hex prefix of SHA-256 of sorted `key=value\n` pairs from the identity tuple (see `store.TupleHash`)
- `container` — container name as it appears in the pod spec
- `resource` — `cpu`, `memory`, or `ephemeral-storage`

Each metric key is a **Redis list** (not a sorted set). Values are string-encoded integers: millicores for CPU, bytes for memory/ephemeral-storage. RPUSH appends; LTRIM enforces the reservoir cap (oldest entries dropped). Lists preserve duplicates, which is essential — identical consecutive values (e.g., CPU always measuring at `1m`) must each count as a distinct sample.

Each metric key also has a companion `{key}:first_seen` key storing the Unix timestamp (ms) of the first sample, set via SET NX. This gives a wall-clock time span for readiness evaluation without storing a timestamp in every list entry. `AllKeysForHash` returns both the list key and its `:first_seen` key so orphan cleanup deletes the full set.

Store API in `internal/store/metrics.go`:
- `AddSample(ctx, c, key, timestampMs, valueStr, cap)` — RPUSH + LTRIM (if cap > 0) + SET NX first_seen
- `QueryAll(ctx, c, key) ([]string, error)` — LRANGE 0 -1
- `FirstSeenMs(ctx, c, key) (int64, error)` — GET key:first_seen; returns 0 if absent
- `SampleCount(ctx, c, key) (int64, error)` — LLEN
- `DeleteKey(ctx, c, key) error` — DEL

Key helpers in `internal/store/`:
- `TupleHash(labels map[string]string) string` — deterministic hash
- `MetricKey(tupleHash, container, resource string) string` — full key
- `AllKeysForHash(ctx, client, tupleHash) ([]string, error)` — scans with `SCAN` + prefix match; returns both list keys and their `:first_seen` companions

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
| `make docker-kind KIND_CLUSTER=<name>` | Build image for host arch (auto-detected via `uname -m`) tagged `:local` and load into the named kind cluster |
| `make helm-install-local` | Install/upgrade chart into the current kubeconfig cluster using the locally loaded `:local` image (`pullPolicy: Never`) |
| `make helm-update-local KIND_CLUSTER=<name>` | Combined: `docker-kind` + `helm-install-local` in one step — the normal local dev iteration command |

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
