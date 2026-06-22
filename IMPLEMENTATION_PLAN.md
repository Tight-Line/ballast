# Ballast — Implementation Plan

## How to Resume a Session

1. Read `DESIGN.md` for full system context (always do this first).
2. Find the first phase below that is not marked `[x] Complete`.
3. Read the **Key files** listed for that phase and each phase it depends on.
4. Implement, run `make check`, open a PR.
5. Give the user instructions for local testing.
6. After the user approves, mark the phase `[x] Complete`, fill in any missing key-file entries, commit the updated plan, and stop — the next session picks up from here.

**A phase is complete when:** (a) `make check` passes, (b) a PR is open with all CI gates green, and (c) the user has approved the behavior by manual testing or inspection.

---

## Status Legend

- `[ ]` Not started
- `[~]` In progress — PR open (link in phase)
- `[x]` Complete — PR merged, user-approved

---

## Phase 1 — Repository Setup & kubebuilder Scaffold

**Status:** `[x]`
**Depends on:** nothing
**PR:** https://github.com/Tight-Line/ballast/pull/1

### What to build

- Run `kubebuilder init --domain tightlinesoftware.com --repo github.com/tight-line/ballast`
- Adjust the generated `Makefile` to match the gatekeeper target set:
  `build`, `test`, `test-coverage`, `test-coverage-check`, `lint`, `lint-fix`, `fmt`, `tidy`, `tools`, `docker`, `setup-hooks`, `check`
- Port `scripts/check-coverage.sh` from gatekeeper (same 100%-with-`coverage:ignore` policy)
- Port `scripts/make-tag` from gatekeeper
- `.golangci.yml` — linter config (same ruleset as gatekeeper)
- `Dockerfile` — distroless base, single binary `ballastd`
- `.github/workflows/ci.yml` — runs `make check` on every PR
- `.github/workflows/pr-images.yml` — builds and pushes `ghcr.io/tight-line/ballast:pr-<number>-<sha>` on every PR push
- `AGENTS.md` skeleton (section headings only; content filled in as phases complete)
- `CHANGELOG.md` with `[Unreleased]` section
- `cmd/ballastd/main.go` — empty `main()` that creates a kubebuilder manager and exits cleanly

### Key files

- `cmd/ballastd/main.go` — kubebuilder manager entry point; registers controllers and webhook (populated in later phases)
- `Makefile` — build/test/lint/check targets; `make check` is the release gate
- `scripts/check-coverage.sh` — 100% coverage enforcement with `coverage:ignore` support; excludes `cmd/`, `test/`, and `e2e` packages; always generates `coverage.filtered.out` for Codecov
- `scripts/make-tag` — creates a semver release tag (runs `make check`, bumps `charts/ballast/Chart.yaml`, moves CHANGELOG section, creates git tag)
- `scripts/pre-commit` — pre-commit hook (fmt check + golangci-lint); installed via `make setup-hooks`
- `.golangci.yml` — linter config; `goimports` local-prefixes set to `github.com/tight-line/ballast`; `test/` excluded from lint
- `Dockerfile` — distroless base, single binary `ballastd`, `VERSION` build arg
- `.github/workflows/ci.yml` — parallel test/lint/build on every PR and main push; uploads `coverage.filtered.out` to Codecov
- `.github/workflows/pr-images.yml` — builds `ghcr.io/tight-line/ballast:pr-<n>-<sha>` on PR push
- `.github/workflows/snyk.yml` — dependency vulnerability scan (high+ severity) on PRs, main, and weekly
- `.github/workflows/sonar.yml` — SonarCloud static analysis on PRs and main
- `sonar-project.properties` — SonarCloud project config (key `Tight-Line_ballast`, org `tight-line`)
- `README.md` — project overview, annotation contract, kill switch, dry-run, dev workflow, implementation status table
- `AGENTS.md` — skeleton with section headings (content filled in as phases complete)
- `CHANGELOG.md` — empty `[Unreleased]` section

### User testing instructions

Confirm `make check` passes on a clean clone; confirm CI workflow runs on the PR with test/lint/build all green; confirm PR image is built and pushed to `ghcr.io/tight-line/ballast:pr-1-<sha>`; confirm Snyk and Codecov runs complete.

---

## Phase 2 — CRD Type Definitions

**Status:** `[x]`
**Depends on:** Phase 1
**PR:** https://github.com/Tight-Line/ballast/pull/2

### What to build

Run `kubebuilder create api --group ballast --version v1 --kind <Kind> --resource --no-controller` for each kind below (controllers come later):

- `MetricsSource` (cluster-scoped)
- `ClusterResourcePolicy` (cluster-scoped)
- `ResourcePolicy` (namespace-scoped)
- `WorkloadProfile` (cluster-scoped, status subresource, no spec)
- `BallastConfig` (cluster-scoped, singleton)

Fill in all Go struct fields per `DESIGN.md`:

- `MetricsSource`: `spec.type`, `spec.config` (pollInterval, reservoirSize)
- `ClusterResourcePolicy` / `ResourcePolicy`: full selector types (kinds, namespace include/exclude regex, annotation map, labelSelector), full metrics slice (resource, field, source, aggregation, headroom), readiness (minDataPoints, minTimeSpan, maxCV), behaviors (thresholds with full coalesce hierarchy, resize.maxChangePerCycle, resize.interval, eviction.cooldown, eviction.maxConcurrentEvictions, eviction.minOtherHealthyReplicas)
- `WorkloadProfile`: status only — tupleLabels, containers (usageStats per resource/source, recommendations per resource, meetsThreshold), activeWorkloads, conditions (Ready, Orphaned)
- `BallastConfig`: identityLabels, orphanTTL, retentionWindow, suspended

Add kubebuilder markers: `+kubebuilder:validation:*`, `+kubebuilder:printcolumn:*`, `+kubebuilder:subresource:status` where applicable.

Unit-test the annotation-combination validator (`internal/validation/annotations.go`) in isolation — this logic is used by both the admission webhook and the WorkloadWatcher; test it here while the types are fresh.

Run `make manifests generate` to produce CRD YAML and deepcopy code.

### Key files (fill in after complete)

- `api/v1/metricssource_types.go`
- `api/v1/clusterresourcepolicy_types.go`
- `api/v1/resourcepolicy_types.go`
- `api/v1/workloadprofile_types.go`
- `api/v1/ballastconfig_types.go`
- `api/v1/zz_generated.deepcopy.go`
- `config/crd/bases/` (generated CRD manifests)
- `internal/validation/annotations.go`
- `internal/validation/annotations_test.go`

### User testing instructions

_Inspect generated CRD YAML; confirm all fields from DESIGN.md are present with correct types and validation markers._

---

## Phase 3 — Logger Infrastructure & Kill Switch

**Status:** `[ ]`
**Depends on:** Phase 2
**PR:** —

### What to build

**Logger (`internal/logger/`):**
- `logger.go` — factory that accepts a component name and level string; returns a `logr.Logger` backed by zap. Supports JSON and text formats.
- CLI flags registered in `cmd/ballastd/main.go`: `--log-level` (global default), `--log-level-webhook`, `--log-level-watcher`, `--log-level-collector`, `--log-level-adjuster`, `--log-format` (json|text)
- Component flags override the global default; absent component flags inherit it.

**Kill switch (`internal/killswitch/`):**
- `killswitch.go` — watches two trigger sources:
  1. A ConfigMap named `ballast-kill-switch` in the operator namespace (presence = active)
  2. `BallastConfig.spec.suspended == true`
- Exposes `IsActive() bool` — returns true if either trigger is active
- Exposes `Reason() string` — returns which trigger is active (for structured log field `kill_switch_reason`)
- All controllers and the webhook call `IsActive()` before taking any external action; log at `warn` with `kill_switch: true` when suppressed
- Unit tests cover: ConfigMap present, ConfigMap absent, BallastConfig.suspended true/false, both active simultaneously, controller-runtime envtest

### Key files

- `internal/logger/logger.go` — `New(component, level, format)` returns a `logr.Logger` backed by zap; `newWithWriter` is the testable variant
- `internal/logger/logger_test.go`
- `internal/killswitch/killswitch.go` — `KillSwitch` reconciler; `IsActive()`/`Reason()` are the hot-path call sites; `SetupWithManager` wires ConfigMap + BallastConfig watches
- `internal/killswitch/killswitch_test.go` — fake-client unit tests covering all trigger combinations
- `cmd/ballastd/main.go` — `--log-level`, `--log-level-{webhook,watcher,collector,adjuster}`, `--log-format`, `--operator-namespace` flags; kill switch registered with manager

### User testing instructions

_After PR opens: run `ballastd --log-level=debug --log-format=text`; confirm per-component flags work. Create the kill-switch ConfigMap; confirm log output shows suppression._

---

## Phase 4 — Policy Resolution

**Status:** `[ ]`
**Depends on:** Phase 2
**PR:** —

### What to build

- `internal/policy/resolver.go` — given a pod's namespace, kind, labels, and annotations, returns the single effective `ClusterResourcePolicy` or `ResourcePolicy` (or nil if none match)
  - Fetches all policies from the controller-runtime cache (no live API calls)
  - Selector matching:
    - `kinds`: pod's owner kind must appear in the list (or list is empty)
    - `namespaces.include`: pod namespace must match the regex (absent = all pass)
    - `namespaces.exclude`: pod namespace must NOT match any entry
    - `annotations`: each key in the map must exist on the pod with a value matching the regex
    - `labelSelector`: standard `metav1.LabelSelector` evaluated against pod labels
  - Precedence: any `ResourcePolicy` beats any `ClusterResourcePolicy`; within same class, highest `priority` wins; tiebreak alphabetical by name
- `internal/policy/resolver_test.go` — comprehensive unit tests:
  - Namespace include/exclude regex
  - Annotation regex matching
  - labelSelector matching
  - ResourcePolicy beats ClusterResourcePolicy regardless of priority
  - Priority ordering within same class
  - Alphabetical tiebreak
  - No-match returns nil

### Key files (fill in after complete)

- `internal/policy/resolver.go`
- `internal/policy/resolver_test.go`

### User testing instructions

_`make check` passing with full test coverage is the gate here; no runtime testing needed for this phase._

---

## Phase 5 — Redis/Valkey Client Layer

**Status:** `[ ]`
**Depends on:** Phase 2
**PR:** —

### What to build

- `internal/store/client.go` — thin wrapper around `go-redis/v9`; configurable via URL string (supports auth, TLS). Exposes a `Client` interface so tests can swap in miniredis.
- `internal/store/keys.go`:
  - `TupleHash(labels map[string]string) string` — deterministic hash of sorted key=value pairs (SHA-256 prefix, hex-encoded)
  - `MetricKey(tupleHash, container, resource string) string` — produces `ballast:metrics:{hash}:{container}:{resource}`
  - `AllKeysForHash(ctx, client, tupleHash) ([]string, error)` — scans for all keys matching a hash prefix (used for profile deletion cleanup)
- `internal/store/metrics.go`:
  - `AddSample(ctx, key string, timestampMs int64, valueStr string) error` — `ZADD`
  - `QueryWindow(ctx, key string, startMs, endMs int64) ([]ScoredValue, error)` — `ZRANGEBYSCORE`
  - `ExpireOlderThan(ctx, key string, cutoffMs int64) error` — `ZREMRANGEBYSCORE`
  - `SampleCount(ctx, key string) (int64, error)` — `ZCARD`
  - `TimeRange(ctx, key string) (firstMs, lastMs int64, err error)` — `ZRANGE ... WITHSCORES` for first and last entries
  - `DeleteKey(ctx, key string) error` — `DEL`
  - `EnforceReservoirCap(ctx, key string, maxEntries int64) error` — `ZREMRANGEBYRANK 0 -(maxEntries+1)` trims oldest entries when cap exceeded
- `internal/store/percentiles.go`:
  - `ComputeStats(values []int64) Stats` — computes p50/p95/p99/max/mean/stddev/CV from a sorted int64 slice
  - `Stats` struct with all fields
- All tests use miniredis; Redis-behavior-specific paths excluded with `coverage:ignore`

### Key files (fill in after complete)

- `internal/store/client.go`
- `internal/store/keys.go`
- `internal/store/keys_test.go`
- `internal/store/metrics.go`
- `internal/store/metrics_test.go`
- `internal/store/percentiles.go`
- `internal/store/percentiles_test.go`

### User testing instructions

_`make check` passing with full test coverage is the gate here._

---

## Phase 6 — Plugin Interface & `kubernetesMetrics` Plugin

**Status:** `[ ]`
**Depends on:** Phase 5
**PR:** —

### What to build

- `internal/plugin/plugin.go` — interface and shared types:
  ```go
  type MetricsPlugin interface {
      Type() string
      FetchStats(ctx context.Context, id WorkloadIdentity, window TimeWindow) ([]ContainerStats, error)
  }
  type WorkloadIdentity struct{ Labels map[string]string }
  type TimeWindow struct{ Start, End time.Time }
  type ContainerStats struct{ ContainerName, Resource string; P50, P95, P99, Max, Mean, StdDev resource.Quantity; SampleCount int64; FirstSeen, LastSeen time.Time }
  ```
- `internal/plugin/registry.go` — `Register(plugin)`, `Get(typeName) (MetricsPlugin, bool)`; plugins register themselves via `init()`.
- `internal/plugin/kubernetes/plugin.go` — `kubernetesMetrics` implementation:
  - Uses the metrics API client (`k8s.io/metrics/pkg/client/clientset/versioned`) to call `PodMetricses` in the namespaces where the identity tuple's pods live
  - Filters to regular containers only (skips initContainers; ephemeral containers are not in `PodMetrics`)
  - Aggregates per-container usage across all pods matching the identity labels into a single `ContainerStats` per container name
  - **Rate limiting:** a token-bucket rate limiter (configurable RPS ceiling via `MetricsSource.spec.config`) shared across all concurrent polls for this plugin instance; requests block until a token is available
  - **Jitter:** each profile's poll goroutine starts with a random delay in `[0, pollInterval)` to spread initial burst
  - **Backoff:** exponential backoff (base 1s, max configurable, default ceiling 5m) on API errors; skips the current cycle rather than queuing behind a slow API server
  - `reservoirSize` is enforced by calling `store.EnforceReservoirCap` after each write
- Tests: mock the metrics API client; test rate limiting, backoff, container filtering, and aggregation

### Key files (fill in after complete)

- `internal/plugin/plugin.go`
- `internal/plugin/registry.go`
- `internal/plugin/kubernetes/plugin.go`
- `internal/plugin/kubernetes/plugin_test.go`

### User testing instructions

_`make check` passing is the gate. If a test cluster with metrics-server is available, manually verify that `FetchStats` returns non-zero values for a running pod._

---

## Phase 7 — WorkloadWatcher Controller

**Status:** `[ ]`
**Depends on:** Phases 3, 4, 5
**PR:** —

### What to build

- `internal/controller/workloadwatcher/controller.go`:
  - Watches pods using a predicate that passes only pods carrying at least one Ballast **behavior** annotation (`ballast.tightlinesoftware.com/measure`, `apply`, `resize`, `evict`, `autoresize`, or `automagic`). The `profile-ref` annotation (set by Ballast itself) is excluded from the predicate to avoid self-triggering. Identity labels are not part of the predicate — they are read during processing.
  - **On pod CREATE/UPDATE (new pod):**
    1. Reads `BallastConfig` to get `identityLabels`
    2. Extracts identity tuple from pod labels using `identityLabels` as the key list; logs a warning and skips if any required label is absent. Derives `WorkloadProfile` name (sorted `key--value--...` from the label values).
    3. Creates `WorkloadProfile` if absent (with `tupleLabels` in status)
    4. Increments `activeWorkloads`; clears `Orphaned` condition if set
    5. Stamps pod with annotation `ballast.tightlinesoftware.com/profile-ref: <profile-name>` (server-side apply patch)
    6. Skip all of the above if kill switch is active; log at `warn`
  - **On pod DELETE:**
    1. Reads `profile-ref` annotation from the pod (uses cached pod object; does not recompute tuple)
    2. Decrements `activeWorkloads` on the referenced `WorkloadProfile`
    3. If `activeWorkloads` reaches 0: sets `Orphaned` condition with `lastTransitionTime = now`
    4. Kill switch does NOT suppress decrement — accounting must stay correct regardless
  - **Orphan TTL (triggered on each WorkloadProfile reconcile):**
    1. If `Orphaned` condition is true and `now - lastTransitionTime >= BallastConfig.orphanTTL`
    2. Calls `store.AllKeysForHash` and `store.DeleteKey` to purge Redis data
    3. Deletes the `WorkloadProfile` object
  - envtest-based tests covering: create, increment, decrement, orphan transition, orphan TTL deletion, Redis key cleanup, kill switch suppression

### Key files (fill in after complete)

- `internal/controller/workloadwatcher/controller.go`
- `internal/controller/workloadwatcher/controller_test.go`

### User testing instructions

_Deploy to a test cluster with a pod carrying the `measure` annotation. Confirm `WorkloadProfile` is created. Delete the pod; confirm `activeWorkloads` decrements and `Orphaned` is set._

---

## Phase 8 — MetricsCollector Controller

**Status:** `[ ]`
**Depends on:** Phases 3, 4, 5, 6, 7
**PR:** —

### What to build

- `internal/controller/metricscollector/controller.go`:
  - Reconciles `WorkloadProfile` objects on a timer (interval from matched policy's `MetricsSource.spec.config.pollInterval`; uses `ctrl.Result{RequeueAfter: interval}`)
  - For each profile:
    1. Find matching policy (via `policy.Resolver`)
    2. Resolve `MetricsSource` from policy; look up plugin from registry
    3. Call `plugin.FetchStats(ctx, identity, window)` where window = `[now - retentionWindow, now]`
    4. For each `ContainerStats` returned: call `store.AddSample` for each metric value; call `store.ExpireOlderThan` for the retention cutoff; call `store.EnforceReservoirCap`
    5. Query Redis: `store.QueryWindow` for the full retention window; compute stats via `store.ComputeStats`
    6. Evaluate readiness: `sampleCount >= minDataPoints`, `timeSpan >= minTimeSpan`, `CV <= maxCV` — all must pass
    7. If ready: compute recommendations for each (resource, field) pair: `aggregatedValue * headroom`
    8. Update `WorkloadProfile` status: `usageStats`, `recommendations`, `meetsThreshold`, conditions
  - **Dry-run (`--dry-run-measure`):** steps 4 and 8 are skipped; log at `info` with `dry_run: true` what would have been written
  - **Kill switch:** steps 4 and 8 are skipped; log at `warn` with `kill_switch: true`
- `internal/stats/aggregator.go`:
  - `EvaluateReadiness(stats Stats, readiness ReadinessConfig) bool`
  - `ComputeRecommendation(stats Stats, metric MetricConfig) resource.Quantity`
- envtest + miniredis tests

### Key files (fill in after complete)

- `internal/controller/metricscollector/controller.go`
- `internal/controller/metricscollector/controller_test.go`
- `internal/stats/aggregator.go`
- `internal/stats/aggregator_test.go`

### User testing instructions

_Deploy to test cluster. After one `pollInterval`, confirm `WorkloadProfile` status shows populated `usageStats`. After `minDataPoints` samples, confirm `meetsThreshold: true` and `recommendations` are populated._

---

## Phase 9 — Admission Webhook

**Status:** `[ ]`
**Depends on:** Phases 3, 4, 7, 8
**PR:** —

### What to build

- `internal/webhook/pod_mutator.go` — implements `admission.Handler` for pod CREATE:
  1. **Kill switch:** if active, return allow without mutation; log `warn`
  2. **Annotation validation:** call `validation.ValidateAnnotations`; if invalid, return deny with descriptive message
  3. **Resolve progressive mode:** if `autoresize` or `automagic` annotation set, read `WorkloadProfile.meetsThreshold`; if false, treat as `measure`-only; if true, treat as `apply+resize` (autoresize) or `apply+resize+evict` (automagic)
  4. **Apply path:** if effective `apply` is active and `WorkloadProfile.meetsThreshold` is true:
     - Resolve policy via `policy.Resolver`
     - For each regular container in the pod spec, patch `resources.requests` and `resources.limits` per `WorkloadProfile.status.recommendations`; skip containers not present in recommendations
     - Record applied values as pod annotations (`ballast.tightlinesoftware.com/applied-cpu-request`, etc.)
  5. **Dry-run (`--dry-run-apply`):** skip the patch; log at `info` with `dry_run: true` what would have been applied
  6. Return allow (with or without patch)
- TLS setup: the webhook server reads cert/key from a mounted Secret (cert-manager provisions it). For envtest, generate a self-signed cert in `TestMain`.
- Register handler in `cmd/ballastd/main.go` under the kubebuilder manager.
- envtest-based tests:
  - Annotation rejection cases (all invalid combos from DESIGN.md)
  - Kill switch passthrough
  - Dry-run passthrough
  - Successful patch (verify container resources are updated)
  - `meetsThreshold: false` → no patch even with `apply` annotation
  - autoresize/automagic: before threshold (measure-only), after threshold (full behavior)

### Key files (fill in after complete)

- `internal/webhook/pod_mutator.go`
- `internal/webhook/pod_mutator_test.go`
- `cmd/ballastd/main.go` (webhook registration)

### User testing instructions

_Deploy to test cluster with cert-manager. Create a pod with `ballast.tightlinesoftware.com/apply: "true"`. Confirm resources are patched (or not, if profile not ready). Try an invalid annotation combo; confirm rejection with clear message._

---

## Phase 10 — ResourceAdjuster Controller

**Status:** `[ ]`
**Depends on:** Phases 3, 4, 7, 8, 9
**PR:** —

### What to build

- `internal/controller/resourceadjuster/controller.go`:
  - Watches `WorkloadProfile` status changes (triggered when MetricsCollector updates status)
  - Re-evaluates on `behaviors.resize.interval` timer
  - **Drift detection:** for each container/resource/field in `recommendations`, compare current pod value to recommended value; compute drift as `|current - recommended| / recommended`; look up threshold via coalesce order (`resourceThresholds -> behavior.default -> global default`); trigger if drift exceeds threshold for any field
  - **Resize path (if `resize` annotation present and drift exceeds resize threshold):**
    1. Cap adjustment to `maxChangePerCycle` relative to current value
    2. Patch pod via `resize` subresource (`v1.Pod` resize API, Kubernetes 1.35+)
    3. On success: record in pod annotation and profile status
    4. On failure (node pressure / infeasible): fall through to eviction check
  - **Eviction path (if `evict` annotation present, or resize blocked):**
    1. Check `minOtherHealthyReplicas`: count ready pods in the same workload (same namespace + same owner Deployment/StatefulSet only; pods from other workloads sharing the same WorkloadProfile are not counted); skip if fewer than the minimum would remain after eviction
    2. Check PDB: attempt Eviction API dry-run; if it returns 429, PDB blocks — skip
    3. Check per-workload cooldown: read last eviction timestamp for this `(namespace, owner-kind, owner-name)` from a map in the WorkloadProfile status; skip if `now - last < cooldown`. Each workload has its own independent clock — workloads sharing the same profile are not affected by each other's cooldowns.
    4. Check `maxConcurrentEvictions`: count pods **cluster-wide** (all namespaces) with matching `profile-ref` that are terminating (`deletionTimestamp != nil`) or not yet ready; skip if count >= limit
    5. If all pass: evict via Eviction API; record timestamp in the per-workload cooldown map in profile status
    6. If any check fails: emit Kubernetes Event, record blocked state in profile status, requeueAfter cooldown
  - **`autoresize` / `automagic`:** once `WorkloadProfile.meetsThreshold` transitions from false to true, the progressive annotations automatically enable resize (autoresize) or resize+evict (automagic); controller respects this dynamically per reconcile
  - **Dry-run:** `--dry-run-resize` suppresses the resize patch; `--dry-run-evict` suppresses eviction; both log at `info` with `dry_run: true`
  - **Kill switch:** suppresses all action
  - envtest tests: drift detection, resize, eviction (all guard conditions), cooldown, concurrent eviction limit, autoresize/automagic threshold flip

### Key files (fill in after complete)

- `internal/controller/resourceadjuster/controller.go`
- `internal/controller/resourceadjuster/controller_test.go`

### User testing instructions

_Deploy to test cluster with a pod carrying `resize` and `evict` annotations and a ready profile. Artificially update the profile's recommendations to force drift beyond threshold. Confirm resize attempt is made. Confirm eviction is issued when resize is blocked._

---

## Phase 11 — Helm Chart

**Status:** `[ ]`
**Depends on:** all prior phases (chart reflects final binary)
**PR:** —

### What to build

`charts/ballast/` containing:

- `Chart.yaml` — `apiVersion: v2`, dependencies include `bitnami/valkey` (with `condition: valkey.enabled`)
- `values.yaml` — all configurable settings with defaults matching DESIGN.md:
  - `image.repository`, `image.tag`, `image.pullPolicy`
  - `replicaCount: 1`
  - `logging.level`, `logging.webhook`, `logging.watcher`, `logging.collector`, `logging.adjuster`, `logging.format`
  - `dryRun.measure`, `dryRun.apply`, `dryRun.resize`, `dryRun.evict` (all false)
  - `ballastConfig.identityLabels`, `ballastConfig.orphanTTL`, `ballastConfig.retentionWindow`
  - `valkey.enabled: true`, `valkey.architecture: replication`
  - `store.endpoint` (used when `valkey.enabled: false`)
  - `certManager.enabled: true`, `certManager.issuerRef`
- `templates/deployment.yaml` — mounts cert Secret; passes all flags from values
- `templates/serviceaccount.yaml`, `templates/clusterrole.yaml`, `templates/clusterrolebinding.yaml` — exact permissions: CRD read/write for all Ballast types, Pod get/list/watch/patch (for resize and eviction), ConfigMap get/watch (kill switch), Event create
- `templates/mutatingwebhookconfiguration.yaml` — `failurePolicy: Fail`; cert-manager `caBundle` injection annotation (`cert-manager.io/inject-ca-from`)
- `templates/certificate.yaml`, `templates/issuer.yaml` — cert-manager resources (rendered when `certManager.enabled: true`)
- `templates/ballastconfig.yaml` — creates the `BallastConfig` singleton from values
- `crds/` — CRD manifests (copied from `config/crd/bases/` at build time)

`helm lint` must pass. Smoke test: `helm template . | kubectl apply --dry-run=client -f -` produces no errors.

### Key files (fill in after complete)

- `charts/ballast/Chart.yaml`
- `charts/ballast/values.yaml`
- `charts/ballast/templates/deployment.yaml`
- `charts/ballast/templates/clusterrole.yaml`
- `charts/ballast/templates/mutatingwebhookconfiguration.yaml`
- `charts/ballast/templates/ballastconfig.yaml`
- `charts/ballast/crds/`

### User testing instructions

_Install chart into a kind cluster with cert-manager pre-installed. Confirm operator pod starts healthy. Confirm `BallastConfig` singleton exists. Create a test pod with `measure` annotation; confirm `WorkloadProfile` is created._

---

## Phase 12 — Polish & Release Readiness

**Status:** `[ ]`
**Depends on:** all prior phases
**PR:** —

### What to build

- `AGENTS.md` complete: all file locations, key functions, build commands, coding standards, testing workflow, PR image workflow, provider development guide
- `README.md`: prerequisites (cert-manager, Kubernetes 1.35+, metrics-server), Helm quickstart, first annotation walkthrough, verifying a `WorkloadProfile`, kill switch usage
- `CHANGELOG.md` `[Unreleased]` section fully populated
- Verify `.github/workflows/pr-images.yml` produces correct image tags end-to-end
- `make check` clean on main branch
- This plan updated with all key-file entries filled in

### User testing instructions

_Follow the README quickstart against a real cluster from scratch. Confirm every step works as written._

---

## Dependency Graph

```
Phase 1 (scaffold)
  └─ Phase 2 (CRD types)
       ├─ Phase 3 (logger + kill switch)
       ├─ Phase 4 (policy resolution)
       └─ Phase 5 (Redis/Valkey layer)
            └─ Phase 6 (plugin interface + kubernetesMetrics)
                 └─ Phase 7 (WorkloadWatcher) ← also needs 3, 4, 5
                      └─ Phase 8 (MetricsCollector) ← also needs 3, 4, 5, 6
                           └─ Phase 9 (Admission webhook) ← also needs 3, 4, 7
                                └─ Phase 10 (ResourceAdjuster) ← also needs 3, 4, 7, 8, 9
                                     └─ Phase 11 (Helm chart)
                                          └─ Phase 12 (polish)
```

Phases 3, 4, and 5 can run in parallel after Phase 2 is complete. Phases 6 and 7 can start once their dependencies are done; they are independent of each other.
