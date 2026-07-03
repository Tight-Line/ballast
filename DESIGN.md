# Ballast — Design Document

Ballast is a Kubernetes operator that automatically right-sizes workload resource requests and limits based on real operational history. It is a more active alternative to Fairwinds Goldilocks: rather than suggesting changes, it applies them — at admission time and on running pods via in-place resize (Kubernetes 1.35+). Cluster rebalancing (pod eviction to force rescheduling) is deliberately out of scope and delegated to [Kubernetes Descheduler](https://github.com/kubernetes-sigs/descheduler), which has the cluster-level view needed to make sound eviction decisions.

---

## Motivation

Applications are deployed with manually-specified resource requests and limits that developers set once and rarely revisit. Over time these drift from reality: dev namespaces are over-provisioned (resource settings copied from production values that were never revisited), prod namespaces become under-provisioned as the application grows new features, and the cluster scheduler makes suboptimal decisions throughout. Ballast closes the loop by observing real utilization, accumulating history keyed to a workload identity tuple, and applying evidence-based resource values at deploy time and during the pod lifecycle.

Ballast is intentionally decoupled from any specific deployment tool. That tool's only responsibility is to emit opt-in annotations on pod templates. Ballast does the rest.

---

## Annotation Contract

The deployment tool sets these annotations on pod template specs. They are the opt-in surface; Ballast never acts on a workload that has not been explicitly enrolled.

| Annotation | Meaning |
|---|---|
| `ballast.tightlinesoftware.com/measure: "true"` | Collect metrics for this workload; required for any other behavior |
| `ballast.tightlinesoftware.com/apply: "true"` | Patch resource requests/limits at admission time; requires `measure` |
| `ballast.tightlinesoftware.com/resize: "true"` | Adjust resources on running pods via in-place resize; requires `apply` |
| `ballast.tightlinesoftware.com/autoresize: "true"` | Progressive mode: behaves as `measure` only until the WorkloadProfile meets its history threshold, then automatically behaves as `apply` + `resize` |

The dependency chain for the explicit annotations is `measure` -> `apply` -> `resize`. Invalid combinations are rejected by the admission webhook with a clear error message. `autoresize` is mutually exclusive with `apply` and `resize`.

The following annotation is set by Ballast itself (not by the deployment tool):

| Annotation | Set by | Meaning |
|---|---|---|
| `ballast.tightlinesoftware.com/profile-ref: <name>` | WorkloadWatcher | Records which `WorkloadProfile` this pod was assigned to at creation time; used for correct decrement on deletion |
| `ballast.tightlinesoftware.com/policy-ref: <name>` | Admission webhook; WorkloadWatcher (on policy change) | Records the effective policy for this pod; re-resolved when policies are created, updated, or deleted; empty string means no policy matched |

The deployment tool also sets the identity tuple labels (see WorkloadProfile below). These are distinct from the behavior annotations.

**Scope of measurement.** Enrollment is a whole-pod, opt-in decision, so deciding *which* workloads to enroll stays with the deployment tool — Ballast does not second-guess it with workload-kind heuristics. In particular there is no built-in Job/CronJob carve-out: a long-running Job may legitimately want right-sizing, so the tool simply should not annotate short-lived Job (or CronJob) pod templates that run to completion.

Within an enrolled pod, Ballast measures and resizes only the regular `spec.containers`. The collector excludes all init containers and ephemeral debug containers from measurement, because the apply and resize paths only ever patch `spec.containers` — sampling anything else would produce recommendations that could never be applied. This is deliberately broader than the ideal end state: restartable-init "native sidecar" containers (`restartPolicy: Always`) are long-running and are legitimate right-sizing targets, so the correct axis is run-to-completion vs long-running rather than init vs regular. Treating restartable-init containers as first-class targets requires extending the apply and resize lanes to patch `spec.initContainers`, which is deferred — tracked in issue #30. Until then, all init containers are excluded.

---

## Architecture Overview

Ballast is a single Go binary managed by kubebuilder. It hosts:

- A **mutating admission webhook** (HTTPS; see TLS note below)
- Three **controllers** sharing a single manager and in-cluster cache

### TLS

The Kubernetes API server requires HTTPS for all admission webhook calls regardless of whether the webhook is in-cluster or external — this is enforced at the protocol level and is not configurable. Ballast must therefore present a valid TLS certificate. cert-manager is the recommended way to provision and rotate this certificate, but any mechanism that populates a valid cert/key into the webhook server and keeps the `MutatingWebhookConfiguration`'s `caBundle` in sync will work.

### Backing store

Ballast requires an **HA Redis-compatible data store** (Redis, Valkey, or any compatible interface). Raw time-series metrics live in Redis-protocol sorted sets; computed aggregates and recommendations are cached in `WorkloadProfile` CRD status. The Helm chart ships Valkey by default (configurable to point at an existing instance instead). This document uses "Redis" when referring to the protocol, data model, or commands.

---

## CRDs

### `MetricsSource` (cluster-scoped)

A named, reusable plugin instance. Multiple policies reference the same source by name. Platform team configures one per metrics backend.

```yaml
apiVersion: ballast.tightlinesoftware.com/v1
kind: MetricsSource
metadata:
  name: k8s-metrics
spec:
  type: kubernetesMetrics   # kubernetesMetrics | sigNoz | (other plugins)
  config:
    # kubernetesMetrics uses the in-cluster metrics API; no endpoint required
    pollInterval: "60s"
    reservoirSize: 10000    # hard cap on Redis sorted-set entries per container per metric;
                            # safety bound — natural growth is retentionWindow / pollInterval
```

Cardinality: 1 MetricsSource -> N policies.

---

### `ClusterResourcePolicy` (cluster-scoped) / `ResourcePolicy` (namespace-scoped)

Same spec shape. Namespace-scoped version implicitly restricts to its own namespace. When multiple policies match a workload, any `ResourcePolicy` beats any `ClusterResourcePolicy` regardless of priority. Within the same class, highest `priority` wins. Same-class same-priority ties break alphabetically by name.

`ClusterResourcePolicy` is the normal case — one cluster-wide policy covers all namespaces matching a pattern, including ephemeral namespaces created after the policy is written.

```yaml
apiVersion: ballast.tightlinesoftware.com/v1
kind: ClusterResourcePolicy
metadata:
  name: platform-defaults
spec:
  priority: 100       # higher wins; tiebreak alphabetical by name

  selector:
    kinds: [Deployment, StatefulSet]   # DaemonSets excluded by omission
    namespaces:
      include:                         # list; absent = all namespaces
        - "/.*-prod/"                  # wrap in / for full-string regex
        - "staging"                    # or exact match
      exclude:                         # overrides include; match on both → excluded (WARN)
        - "kube-system"
        - "cert-manager"
        - "monitoring"
    annotations:
      "my.org/business-unit": "/.+/"   # /regex/ or exact match; pod annotation must match
    labelSelector:                    # standard k8s LabelSelector
      matchLabels:
        tier: backend

  metrics:
    # Each entry maps actual resource usage (measured by the plugin) to a
    # specific k8s resource field. The plugin always measures usage, never
    # prior requests or limits. `field` specifies what Ballast will set.
    - resource: cpu
      field: request          # k8s field to set: request | limit
      source: k8s-metrics     # MetricsSource name; provides actual CPU usage data
      aggregation: p95        # percentile of usage: p50 | p95 | p99 | max | avg
      headroom: 1.2           # multiply the usage percentile by this factor
    - resource: cpu
      field: limit
      source: k8s-metrics
      aggregation: p99
      headroom: 1.25
    - resource: memory
      field: request
      source: k8s-metrics
      aggregation: p99
      headroom: 1.1
    - resource: memory
      field: limit
      source: k8s-metrics
      aggregation: p99
      headroom: 1.2
    # resources not listed are left untouched

  readiness:                       # ALL conditions must pass before Ballast will act
    minDataPoints: 500            # default: 500 (~8h at 1 sample/min)
    minTimeSpan: "24h"            # default: 24h (covers one full daily cycle)
    maxCV: 1.5                    # default: 1.5 — permissive; CPU is inherently spiky.
                                  # coefficient of variation (stddev/mean); lower = more stable

  behaviors:
    thresholds:
      default: "20%"              # default: 20% — global fallback for all behaviors and resources
      resize:
        default: "20%"            # default: 20% — overrides global default for resize trigger
        resourceThresholds:       # coalesce: resourceThresholds -> resize.default -> default
          cpu:
            limit: "20%"
            request: "15%"
          memory:
            limit: "10%"
            # memory.request uses resize.default = "20%"
    # a drift in ANY field exceeding its threshold independently triggers the behavior
    resize:
      maxChangePerCycle: "50%"    # default: 50% — cap single-cycle step to this % of the current→recommended gap
      interval: "15m"             # default: 15m — how often ResourceAdjuster re-evaluates
```

Behaviors in the policy are **parameters**, not switches. Whether a workload gets resized is still gated by the `ballast.tightlinesoftware.com/resize` annotation on the pod template. Policy says how; annotations say whether.

Cardinality: N policies -> M workloads (selector match); one workload -> at most one effective policy (ResourcePolicy beats ClusterResourcePolicy; within the same class, highest priority wins).

---

### `WorkloadProfile` (cluster-scoped, operator-owned)

Represents the accumulated operational history for a workload identity tuple. Not per-Deployment — per identity, across all workload instances sharing the same tuple of labels. Users never create or modify these; the operator owns them entirely.

**Identity tuple**

The set of labels that constitute an identity tuple is defined in the Ballast global configuration (Helm values or a `BallastConfig` CRD — TBD). The operator chooses which pod labels to include. Common configurations:

- One label: `app.kubernetes.io/name` — groups all instances of an app regardless of environment
- Two labels: `app.kubernetes.io/name` + `ballast.tightlinesoftware.com/resource-profile` — groups by app and environment type (dev, qa, prod, etc.)
- Three labels: `app.kubernetes.io/name` + `app.kubernetes.io/instance` + `ballast.tightlinesoftware.com/resource-profile` — groups by app, specific instance, and environment type

The deployment tool is responsible for setting whatever labels the operator has configured on pod templates. The `WorkloadProfile` name is derived deterministically from the sorted label values, e.g. `billing--prod`.

The profile for `(billing, prod)` aggregates metrics from every workload in any namespace that carries those label values and has a `measure` annotation. All contributing instances are treated as equivalent samples of the same workload in the same environment type.

```yaml
apiVersion: ballast.tightlinesoftware.com/v1
kind: WorkloadProfile
metadata:
  name: billing--prod
  # no namespace — cluster-scoped
# no spec — operator-managed; users do not write to this object
status:
  tupleLabels:
    app.kubernetes.io/name: billing
    ballast.tightlinesoftware.com/resource-profile: prod
  containers:
    - name: app
      # usageStats: measured actual resource consumption, per resource per source.
      # This is the raw aggregated data; request and limit recommendations are derived from it.
      usageStats:
        - resource: cpu
          source: k8s-metrics
          samples: 14500
          timeSpan: "168h"
          p50: "85m"
          p95: "240m"
          p99: "310m"
          mean: "120m"
          stdDev: "55m"
          cv: "0.46"              # stddev/mean; watch for high values
          lastUpdated: "2026-06-17T14:32:00Z"
      # recommendations: derived from usageStats + matching policy (aggregation * headroom).
      # Cached here so the admission webhook needs only one CRD read.
      recommendations:
        cpu:
          request: "288m"         # p95 * 1.2
          limit: "375m"           # p99 * 1.25
        memory:
          request: "410Mi"
          limit: "450Mi"
      meetsThreshold: true
  state: Sufficient             # Accruing until meetsThreshold, then Sufficient (shown by kubectl get)
  activeWorkloads: 3            # number of workloads currently contributing
  conditions:
    - type: Ready               # collection health: every policy resource produced at least
                                # one sample in the latest cycle; orthogonal to meetsThreshold
      status: "True"
      reason: SamplesCollected
    - type: Orphaned            # true when activeWorkloads == 0
      status: "False"
      lastTransitionTime: "2026-06-17T14:30:00Z"
      # orphanTTL is measured from lastTransitionTime when Orphaned is True
```

**Lifecycle:**

- Created by the operator when a new identity tuple is first observed.
- `activeWorkloads` is decremented when a contributing workload disappears.
- When `activeWorkloads` reaches 0, the profile is marked `Orphaned` but not immediately deleted. Old data is better than no data if the workload reappears.
- A configurable `orphanTTL` (default: 7 days) triggers deletion after the profile has been orphaned for that duration.
- Stale samples in Redis are expired via TTL independent of profile lifecycle.

**Access control:** RBAC is the protection mechanism. The operator's ServiceAccount has full write access; user-facing ServiceAccounts are granted read-only access at most. A validating webhook is not needed: any manual edit to a user-writable field is overwritten on the operator's next reconcile anyway.

---

### `BallastConfig` (cluster-scoped, singleton)

Global runtime configuration. The Helm chart creates exactly one instance named `ballast` at install time, populated from Helm values. Operators patch it directly thereafter — no Helm upgrade required for runtime adjustments.

```yaml
apiVersion: ballast.tightlinesoftware.com/v1
kind: BallastConfig
metadata:
  name: ballast   # singleton; only one instance is honored
spec:
  # Ordered list of pod label keys that define a WorkloadProfile identity tuple.
  # Must be set at install time. Changing this after workloads are enrolled
  # invalidates existing WorkloadProfile names and requires a migration.
  identityLabels:
    - app.kubernetes.io/name
    - ballast.tightlinesoftware.com/resource-profile

  # How long to retain an Orphaned WorkloadProfile before deleting it.
  orphanTTL: "168h"         # default: 7 days

  # Default Redis sample retention window (overridable per MetricsSource).
  retentionWindow: "168h"   # default: 7 days

  # Global suspension — identical effect to the emergency kill-switch ConfigMap.
  # Use for planned, GitOps-managed suspensions.
  suspended: false
```

The `identityLabels` field is the only setting that is effectively immutable after initial enrollment: changing it renames all `WorkloadProfile` objects and orphans the old Redis keys. A future migration utility can handle this, but it is not in scope for v1.

---

## Redis Data Model

Raw time-series samples live in Redis sorted sets. The CRD stores computed aggregates only.

**Key schema:**
```
ballast:metrics:{tuple-hash}:{container}:{resource}
```

Where `tuple-hash` is a deterministic hash of the sorted label key-value pairs that constitute the identity tuple. The `WorkloadProfile` status records the full label map so the hash is always traceable.

Example: `ballast:metrics:a3f8c2:app:cpu`

**Value:** sorted set where score = Unix timestamp (milliseconds), member = observed value (millicores or bytes as integer string).

**Operations used:**

- `ZADD` — ingest a new sample
- `ZRANGEBYSCORE` — query a time window for percentile computation
- `ZCARD` — sample count
- `ZREMRANGEBYSCORE` — expire samples older than the retention window
- `ZRANGE ... WITHSCORES` — first/last seen timestamps

**Retention:** configurable per `MetricsSource`, defaulting to 7 days. The MetricsCollector runs `ZREMRANGEBYSCORE` on each sync.

---

## Components

### Admission Webhook

Fires on pod CREATE for pods matching any `ballast.tightlinesoftware.com/*` annotation.

1. Validates annotation combinations; rejects invalid combos with a descriptive message.
2. Resolves the matching policy (by selector + priority).
3. Reads the `WorkloadProfile` status for the pod's identity tuple.
4. If `apply` annotation present and `meetsThreshold: true`: patches resource requests/limits per the cached recommendation.
5. Records what it applied (or why it did not) as annotations on the pod.
6. Never writes to `WorkloadProfile` — read-only with respect to CRDs.

`failurePolicy: Fail`. Admission is blocked if the webhook is unavailable.

---

### WorkloadWatcher Controller

Watches pods with any `ballast.tightlinesoftware.com/*` annotation. Enrollment is
**level-triggered**: every CREATE/UPDATE reconcile recomputes the pod's desired
profile from its *current* labels and the *current* `identityLabels`, then reconciles
toward it, rather than trusting the stamped `profile-ref`. The stamp is a cache used
only on the DELETE path.

- On create/update: if the pod carries a behavior annotation, computes the target profile name, creates the `WorkloadProfile` if absent (recreating it if it was deleted), stamps `ballast.tightlinesoftware.com/profile-ref`, and recomputes `activeWorkloads` from live pods. If the computed name differs from the stamp (a pod-label or `identityLabels` change), the pod **migrates**: it is re-stamped and both the new and old profiles are recounted. If all behavior annotations have been removed, the pod is **un-enrolled**: profile-ref and finalizer removed, old profile decremented.
- On pod deletion: reads the stamped `profile-ref` (does not recompute — the pod is leaving) to identify which `WorkloadProfile` to recount; sets `Orphaned` when `activeWorkloads` reaches 0.
- On any profile deletion (orphan-TTL sweep **or** manual `kubectl delete`): a cleanup finalizer purges the profile's Redis keys before the object is removed. Deleting a profile that still has matching live pods is a history reset — the pod watch promptly recreates a fresh profile and re-associates only matching pods.

The count is level-triggered (recomputed from live pod state each reconcile), so it self-heals against missed or duplicated events. Prompt convergence is driven by watches (WorkloadProfile deletions, `identityLabels` changes) with the informer resync as the correctness backstop.

**See [docs/convergence.md](docs/convergence.md) for the canonical sequence diagrams and design invariants. Update it when changing enrollment or profile-lifecycle behavior.**

---

### MetricsCollector Controller

Periodic reconciliation loop (interval per policy, configurable).

**Container scope:** metrics are collected for regular (non-init, non-ephemeral) containers only. InitContainers complete before the pod reaches `Running` phase and are no longer visible to the metrics API by the time any polling runs; their workload pattern (short-lived setup tasks) is also fundamentally different from steady-state service behavior. Ephemeral containers are debug-only and equally transient. Both are ignored.

For each `WorkloadProfile`:

1. Finds the matching policy.
2. Calls the referenced plugin's `FetchStats` for the profile's identity tuple and configured time window.
3. Writes new samples to Redis via `ZADD`.
4. Runs `ZREMRANGEBYSCORE` to expire old samples.
5. Queries Redis for p50/p95/p99/mean/stddev/count/timespan.
6. Evaluates `readiness` conditions (minDataPoints, minTimeSpan, maxCV).
7. Computes the cached recommendation (aggregation + headroom from policy).
8. Updates `WorkloadProfile` status.

---

### ResourceAdjuster Controller

Watches `WorkloadProfile` status changes.

When any field's recommendation drifts beyond its configured threshold (per `behaviors.thresholds` coalesce order):

1. Finds all running pods for contributing workloads.
2. If `resize` annotation present: patches the pod via the `resize` subresource (Kubernetes 1.35+ in-place resize).
   - Success: records in pod annotation and profile status.
   - Blocked (node cannot satisfy request): emits a Kubernetes Event, records blocked state in profile status, retries after interval.
3. If `resize` is not present: emits a Kubernetes Event noting that drift exceeds threshold but no action is configured.

Pod eviction is not performed by Ballast. For cluster rebalancing, pair Ballast with [Kubernetes Descheduler](https://github.com/kubernetes-sigs/descheduler) (`LowNodeUtilization` strategy). Once Ballast has corrected resource requests via resize, Descheduler can repack the cluster based on those accurate values.

---

## Plugin Interface

```go
type MetricsPlugin interface {
    // Type matches MetricsSource.spec.type
    Type() string

    // FetchStats returns pre-aggregated statistics for the given identity
    // tuple and time window. How aggregation is achieved is internal to
    // the plugin.
    FetchStats(ctx context.Context, id WorkloadIdentity, window TimeWindow) ([]ContainerStats, error)
}

type WorkloadIdentity struct {
    // Labels contains the key-value pairs that constitute the identity tuple,
    // as defined in the Ballast global configuration.
    Labels map[string]string
}

type TimeWindow struct {
    Start time.Time
    End   time.Time
}

type ContainerStats struct {
    ContainerName string
    Resource      string            // "cpu", "memory", "ephemeral-storage"
    Field         string            // "request", "limit"
    P50, P95, P99 resource.Quantity
    Max, Mean     resource.Quantity
    StdDev        resource.Quantity
    SampleCount   int64
    FirstSeen     time.Time
    LastSeen      time.Time
}
```

Plugins are compiled into the binary and registered by type name. Additional plugins (e.g. for Prometheus or VictoriaMetrics backends) can be added without changes to the core operator; they are on the roadmap for v2.

**`kubernetesMetrics` plugin implementation note:** this plugin polls the in-cluster metrics API (metrics-server) on a schedule and writes one sample per container per metric to Redis on each cycle. Samples accumulate in a sliding time window; `ZREMRANGEBYSCORE` trims entries older than `retentionWindow` on every cycle, and `reservoirSize` is a hard cap that bounds Redis growth regardless of poll frequency. The metrics API is a shared cluster resource; the plugin must be careful not to overwhelm it. Implementation requirements:

- Requests are rate-limited and jittered so that a large number of `WorkloadProfile` objects do not produce a burst of simultaneous API calls.
- Exponential backoff with a configurable ceiling is used on errors.
- The `pollInterval` in `MetricsSource.spec.config` is a floor, not a target: the plugin skips a cycle rather than queuing behind a slow or failing API server.
- The `reservoirSize` cap (default 10,000 samples per container per metric) bounds per-profile memory regardless of poll frequency.

**`kubeletSummary` plugin implementation note:** this plugin fetches the kubelet Summary API via the Kubernetes API server proxy (`GET /api/v1/nodes/{name}/proxy/stats/summary`) for each node in parallel and extracts `ephemeral-storage.usedBytes` per pod. Because the Summary API does not include pod labels, the plugin maintains a cluster-wide pod label cache (same TTL as the node summary cache) to resolve labels for client-side filtering. Ephemeral storage is reported at the pod level; the plugin distributes usage evenly across a pod's containers as an approximation — callers should treat per-container values as estimated shares. Per-node caches absorb transient failures: entries between `CacheTTL` and `2×CacheTTL` old are used as stale data on refresh failure; entries older than `2×CacheTTL` are skipped entirely with a warning log. The `nodes/proxy` `get` verb is required in the `ClusterRole`.

---

## Helm Chart

The Ballast Helm chart ships:

- Ballast operator Deployment
- HA Valkey (via subchart; can be disabled in favor of an external Valkey or Redis-compatible endpoint)
- `MutatingWebhookConfiguration`
- RBAC (ClusterRole, ClusterRoleBinding, ServiceAccount)
- CRD manifests
- TLS certificate resources (cert-manager `Certificate` and `Issuer` recommended; any mechanism that keeps the webhook cert and `caBundle` in sync is acceptable)

**Required configuration parameters:**

- Valkey endpoint (if not using bundled Valkey)
- TLS certificate source
- Identity tuple label keys (the ordered list of pod label keys that define a `WorkloadProfile` identity)

---

## Relationship to Deployment Tooling

Ballast has no knowledge of any specific deployment tool's internals. The annotation contract is the only interface.

A deployment tool that wants to integrate with Ballast needs only to:

- Maintain per-environment toggles for the behaviors (measure, apply, resize, autoresize) at whatever granularity makes sense (per-service, per-environment-type, per-specific-environment)
- Emit the corresponding `ballast.tightlinesoftware.com/*` annotations on pod template specs at manifest generation time
- Set whatever pod labels constitute the configured identity tuple on pod templates

---

## Build and Test Harness

Modeled on the [gatekeeper](https://github.com/tight-line/gatekeeper) project.

### Makefile Targets

| Target | What it does |
|---|---|
| `make build` | Build the `ballastd` binary |
| `make test` | Run all tests (`go test -v ./...`) |
| `make test-coverage` | Run tests with coverage report (`coverage.html`) |
| `make test-coverage-check` | Enforce 100% coverage; uncovered lines require `// coverage:ignore - <reason>` |
| `make lint` | Run `golangci-lint` |
| `make lint-fix` | Run `golangci-lint` with auto-fix |
| `make fmt` | Run `go fmt` and `goimports` |
| `make docker` | Build Docker image |
| `make check` | Full pre-release gate: lint + test-coverage-check + build |

`make check` is required before every release and is the gate CI enforces on PRs.

### Coverage Policy

100% test coverage is required. Genuinely untestable lines (e.g., code paths that require simulating Kubernetes API behaviors not reproducible in envtest, or Redis behaviors not reproducible in miniredis) may be excluded with:

```go
// coverage:ignore - <reason>
```

Coverage is enforced by `scripts/check-coverage.sh`, which validates that every uncovered line carries an ignore comment with a reason. The script also supports a `--codecov` flag that generates a filtered coverage file for Codecov integration where ignored lines are reported as covered.

### Testing Strategy

**Controllers** are tested with `controller-runtime`'s `envtest` package, which runs against a real Kubernetes API server binary. This gives realistic CRD and RBAC behavior without a live cluster.

**Admission webhook** is tested against `envtest` as well; the test suite starts the webhook server and registers a `MutatingWebhookConfiguration` pointing at it.

**Redis** is tested with `miniredis` (in-process Redis-compatible server). Code paths that depend on Redis behaviors not reproducible with miniredis are excluded with `coverage:ignore`.

All tests that exercise race-sensitive code run with `-race`. `make test-coverage-check` always enables `-race` and `-covermode=atomic`.

A `ci` build tag enables test helpers that must not ship in the production binary.

### CI

GitHub Actions runs `make check` on every PR. PR Docker images are built and pushed to `ghcr.io/tight-line/ballast:pr-<number>-<sha>` for integration testing against a real cluster.

### Changelog

`CHANGELOG.md` follows the gatekeeper model: an `[Unreleased]` section accumulates entries until a tagged release. `scripts/make-tag` moves them to a dated section and creates the git tag. `make check` must pass before tagging.

---

## Logging

Ballast uses structured logging throughout. The `controller-runtime` logger (`logr` interface, backed by `zap`) is used so log output integrates naturally with kubebuilder scaffolding and cluster log aggregation pipelines.

### Log Levels

Standard levels: `debug` (verbose internals), `info` (normal lifecycle events), `warn` (recoverable anomalies), `error` (failures requiring attention).

### Per-Component Level Control

Each component has its own log level, independently configurable at startup.

| Component | CLI flag | Helm value |
|---|---|---|
| Global default | `--log-level` | `logging.level` |
| Admission webhook | `--log-level-webhook` | `logging.webhook` |
| WorkloadWatcher controller | `--log-level-watcher` | `logging.watcher` |
| MetricsCollector controller | `--log-level-collector` | `logging.collector` |
| ResourceAdjuster controller | `--log-level-adjuster` | `logging.adjuster` |

All flags accept: `debug`, `info`, `warn`, `error`. Component flags override the global default; absent component flags inherit it.

### Log Format

Default: JSON (suitable for log aggregation pipelines). `--log-format=text` enables human-readable output for local development. Configurable via Helm value `logging.format`.

### Structured Fields

All log entries carry a consistent set of fields where applicable:

| Field | Description |
|---|---|
| `namespace` | Pod or workload namespace |
| `pod` | Pod name |
| `profile` | `WorkloadProfile` name |
| `policy` | Matching policy name |
| `action` | `measure`, `apply`, `resize` |
| `dry_run` | `true` when a dry-run flag suppresses the action |
| `kill_switch` | `true` when suppression is due to an active kill switch |

---

## Kill Switch

Since Ballast can modify running workloads, operators need a way to halt all activity immediately — including before the cause of an incident has been diagnosed. The kill switch is designed for use under pressure: one command, no schema knowledge required.

### Emergency Kill Switch (ConfigMap)

Create a ConfigMap named `ballast-kill-switch` in the Ballast operator namespace:

```bash
kubectl create configmap ballast-kill-switch -n ballast-system
```

Presence of this ConfigMap (any content; existence is sufficient) causes:

- The admission webhook to pass all pods through without mutation.
- All controllers to halt external actions: no Redis writes, no resource patches. Reconcile loops continue running (to avoid a thundering herd on resume) but take no action.
- All suppressed actions logged at `warn` level with `kill_switch: true`.

To resume:

```bash
kubectl delete configmap ballast-kill-switch -n ballast-system
```

Controllers pick up the change on their next reconcile (within one poll interval); no restart required.

**`failurePolicy: Fail` interaction:** the admission webhook remains up while the operator is running — it just returns allow-passthrough. Pod creation is not blocked. If the operator itself is scaled to zero, the `MutatingWebhookConfiguration` must be deleted or set to `failurePolicy: Ignore` first, or pod creates will block cluster-wide.

### Scoped Suspension (GitOps-managed)

For planned, GitOps-controlled suspension (maintenance windows, staged rollouts), the `BallastConfig` singleton CRD exposes a `suspended` flag:

```yaml
spec:
  suspended: true
```

Behavior is identical to the emergency kill switch. Both mechanisms are checked independently; either one being active is sufficient to suppress all action.

---

## Dry-run Mode

Dry-run allows operators to validate Ballast's behavior without committing changes. Each action has a dry-run toggle; they cascade because each downstream action depends on the one above it.

### Cascade Rule

```
dry-run: measure
  implies dry-run: apply
    implies dry-run: resize
```

You can widen or narrow dry-run scope from the top. Requesting dry-run for an upstream action (e.g., `measure`) automatically dry-runs everything downstream. Requesting dry-run for a downstream action alone (e.g., `resize`) is also valid — measurement and apply proceed normally, but no resize patches are issued.

### Configuration

Dry-run flags are global (apply to all workloads) and set at the Helm / CLI level.

| Action | CLI flag | Helm value |
|---|---|---|
| measure | `--dry-run-measure` | `dryRun.measure` |
| apply | `--dry-run-apply` | `dryRun.apply` |
| resize | `--dry-run-resize` | `dryRun.resize` |

Any flag also activates all downstream flags implied by the cascade rule. The effective dry-run set is logged at `info` level on startup.

### Behavior Under Dry-run

| Action | Live behavior | Dry-run behavior |
|---|---|---|
| measure | Writes samples to Redis; updates `WorkloadProfile` stats | Computes stats, logs what would be written; no Redis writes, no status update |
| apply | Patches pod resource requests/limits at admission | Logs the patch that would be applied; pod admitted without modification |
| resize | Patches running pod via the resize subresource | Logs the resize that would be issued; pod is not touched |

All dry-run actions are logged at `info` level with `dry_run: true` in structured fields.

---

## Future Considerations

Items deliberately deferred from v1 scope.

- **Per-instance metrics storage:** the current design aggregates all contributing workload instances into a single Redis sorted set per tuple/container/resource. An alternative would key by workload instance (`{tuple-hash}:{workload-id}:{container}:{resource}`) and aggregate at query time, enabling outlier detection and per-instance debugging. Significant added complexity to the Redis data model and query logic; revisit in v2.
- **Container exclusion annotation:** a `ballast.tightlinesoftware.com/exclude-containers: "istio-proxy,fluentd"` annotation to suppress Ballast management for specific sidecar containers. Not needed for v1 (all regular containers are managed); add when operators hit this in practice.
- **Prometheus / VictoriaMetrics plugin:** a metrics plugin that queries an external observability backend rather than the in-cluster metrics API. Richer historical data and no metrics-server dependency. Implement as a second plugin after the `kubernetesMetrics` plugin is stable.
- **`identityLabels` migration utility:** a tool (or operator runbook) for renaming `WorkloadProfile` objects and re-keying Redis data when `BallastConfig.spec.identityLabels` is changed post-enrollment. Out of scope for v1; treat `identityLabels` as immutable after initial rollout.
