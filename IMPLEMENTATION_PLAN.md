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
- `ClusterResourcePolicy` / `ResourcePolicy`: full selector types (kinds, namespace include/exclude regex, annotation map, labelSelector), full metrics slice (resource, field, source, aggregation, headroom), readiness (minDataPoints, minTimeSpan, maxCV), behaviors (thresholds with full coalesce hierarchy, resize.maxChangePerCycle, resize.interval)
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

**Status:** `[x]`
**Depends on:** Phase 2
**PR:** https://github.com/Tight-Line/ballast/pull/3

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

**Status:** `[x]`
**Depends on:** Phase 2
**PR:** https://github.com/Tight-Line/ballast/pull/5

### What to build

**`internal/policy/resolver.go`** — resolves the single effective policy for a given pod against all `ClusterResourcePolicy` and `ResourcePolicy` objects in the controller-runtime cache (no live API calls).

Input type `Input`: `Namespace string`, `OwnerKind string` (pre-resolved by caller — Deployment, StatefulSet, etc.), `Labels map[string]string`, `Annotations map[string]string`.

Return type `*ResolvedPolicy`: `Spec ballastv1.ClusterResourcePolicySpec`, `Name string` (used to stamp `ballast.tightlinesoftware.com/policy-ref`), `Namespaced bool`. Returns nil if no policy matches.

**Callers:** admission webhook (Phase 9) and WorkloadWatcher (Phase 7) only. MetricsCollector and ResourceAdjuster read the `ballast.tightlinesoftware.com/policy-ref` annotation already stamped on the pod at admission time.

**Selector evaluation (all conditions must pass):**
- `kinds`: pod's `OwnerKind` must appear in the list, or list is empty.
- `namespaces.include`: pod namespace must match at least one pattern (empty list = all pass). Patterns wrapped in `/` are full-string anchored regexes (e.g. `/.*-prod/`); all others are exact matches.
- `namespaces.exclude`: pod namespace must NOT match any pattern (same `/regex/` or exact syntax). Matching both include and exclude → excluded, WARN logged.
- `annotations`: each selector key must exist on the pod with a value matching the pattern (same syntax).
- `labelSelector`: standard `metav1.LabelSelector` evaluated against pod labels.

**Precedence:** `ResourcePolicy` beats `ClusterResourcePolicy` regardless of priority. Within the same class, higher `priority` wins; equal priority breaks alphabetically by name.

**`internal/policy/resolver_test.go`** — unit tests using a fake controller-runtime client:
- Namespace include: exact, regex, absent (all pass), list with second entry matching
- Namespace exclude: exact, regex
- Namespace matching both include and exclude → excluded
- Annotation exact match, regex match, key missing, value mismatch
- LabelSelector match, no-match, and invalid expression (returns error)
- `ResourcePolicy` beats `ClusterResourcePolicy` regardless of priority
- Higher priority wins within same class
- Alphabetical tiebreak (both cluster-scoped and namespace-scoped)
- No policies → nil
- Invalid regex in include, exclude, or annotation → error

### Key files

- `internal/policy/resolver.go`
- `internal/policy/resolver_test.go`

### User testing instructions

_`make check` passing with full test coverage is the gate here; no runtime testing needed for this phase._

---

## Phase 5 — Redis/Valkey Client Layer

**Status:** `[x]`
**Depends on:** Phase 2
**PR:** https://github.com/Tight-Line/ballast/pull/6

### What to build

- `internal/store/client.go` — thin wrapper around `go-redis/v9`; configurable via URL string (supports auth, TLS). Exposes a `Client` interface so tests can swap in miniredis.
- `internal/store/keys.go`:
  - `TupleHash(labels map[string]string) string` — deterministic hash of sorted key=value pairs (SHA-256 prefix, hex-encoded)
  - `MetricKey(tupleHash, container, resource string) string` — produces `ballast:metrics:{hash}:{container}:{resource}`
  - `AllKeysForHash(ctx, client, tupleHash) ([]string, error)` — scans for all keys matching a hash prefix (used for profile deletion cleanup)
- `internal/store/metrics.go` (list-based; refined in PR #21):
  - `AddSample(ctx, c, key, timestampMs, valueStr string, cap int64) error` — `RPUSH` + `LTRIM` (reservoir cap) + `SET NX` for `key:first_seen`
  - `QueryAll(ctx, c, key string) ([]string, error)` — `LRANGE 0 -1`
  - `FirstSeenMs(ctx, c, key string) (int64, error)` — `GET key:first_seen`; wall-clock time of first sample for readiness evaluation
  - `SampleCount(ctx, c, key string) (int64, error)` — `LLEN`
  - `DeleteKey(ctx, c, key string) error` — `DEL`
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

**Status:** `[x]`
**Depends on:** Phase 5
**PR:** https://github.com/Tight-Line/ballast/pull/7

### What to build

- `internal/plugin/plugin.go` — interface and shared types:
  ```go
  type MetricsPlugin interface {
      Type() string
      FetchStats(ctx context.Context, id WorkloadIdentity, window TimeWindow) ([]ContainerStats, error)
  }
  type WorkloadIdentity struct{ Labels map[string]string }
  type TimeWindow struct{ Start, End time.Time }
  // ContainerStats is a single point-in-time raw measurement for one pod/container/resource.
  // Statistical aggregation (P50/P95/P99 etc.) happens in the MetricsCollector after
  // samples accumulate in Redis.
  type ContainerStats struct{ ContainerName, Resource string; Value resource.Quantity; Timestamp time.Time }
  ```
- `internal/plugin/registry.go` — `Register(plugin)`, `Get(typeName) (MetricsPlugin, bool)`; plugins register themselves via `init()`.
- `internal/plugin/kubernetes/plugin.go` — `kubernetesMetrics` implementation:
  - Uses `PodMetricsLister` (satisfied by `mc.MetricsV1beta1().PodMetricses("")`) to call the in-cluster metrics API
  - Returns one `ContainerStats` per pod/container/resource; all resources present in the Usage map are included (cpu, memory, ephemeral-storage). The metrics API server omits initContainers and ephemeral containers automatically.
  - **Rate limiting:** token-bucket rate limiter (configurable RPS ceiling) shared across all concurrent polls for this plugin instance; requests block until a token is available
  - **Jitter:** caller's responsibility — MetricsCollector adds per-profile startup jitter before the first poll to spread the initial burst
  - **Backoff:** exponential backoff (base 1s, max configurable, default ceiling 5m) on API errors; returns an error immediately if still in backoff so the caller skips the cycle
- Tests: mock the metrics API client; test rate limiting, backoff, container filtering, and raw measurement collection

### Key files

- `internal/plugin/plugin.go`
- `internal/plugin/registry.go`
- `internal/plugin/registry_test.go`
- `internal/plugin/kubernetes/plugin.go`
- `internal/plugin/kubernetes/plugin_test.go`

### User testing instructions

_`make check` passing is the gate. If a test cluster with metrics-server is available, manually verify that `FetchStats` returns non-zero values for a running pod._

---

## Phase 7 — WorkloadWatcher Controller

**Status:** `[x]`
**Depends on:** Phases 3, 4, 5
**PR:** https://github.com/Tight-Line/ballast/pull/9

### What to build

- `internal/controller/workloadwatcher/controller.go`:
  - Watches pods using a predicate that passes only pods carrying at least one Ballast **behavior** annotation (`ballast.tightlinesoftware.com/measure`, `apply`, `resize`, or `autoresize`). The `profile-ref` annotation (set by Ballast itself) is excluded from the predicate to avoid self-triggering. Identity labels are not part of the predicate — they are read during processing.
  - **On pod CREATE/UPDATE (new pod):**
    1. Reads `BallastConfig` to get `identityLabels`
    2. Extracts identity tuple from pod labels using `identityLabels` as the key list; logs a warning and skips if any required label is absent. Derives `WorkloadProfile` name (`value--value--...` in `identityLabels` declaration order, not alphabetical sort; refined in PR #21).
    3. Creates `WorkloadProfile` if absent (with `tupleLabels` in status)
    4. Increments `activeWorkloads`; clears `Orphaned` condition if set
    5. Stamps pod with annotation `ballast.tightlinesoftware.com/profile-ref: <profile-name>` (server-side apply patch)
    6. Skip all of the above if kill switch is active; log at `warn`
  - **On pod DELETE (refined in PR #21):**
    1. Guard: if our finalizer is absent, return immediately — no decrement. The finalizer is added atomically with the increment, so its presence is the definitive "not yet decremented" signal. This prevents double-decrements when removing the finalizer triggers a second MODIFIED→DELETE reconcile.
    2. Reads `profile-ref` annotation from the pod (uses cached pod object; does not recompute tuple)
    3. Decrements `activeWorkloads` on the referenced `WorkloadProfile`
    4. Removes the finalizer via `client.Update`
    5. If `activeWorkloads` reaches 0: sets `Orphaned` condition with `lastTransitionTime = now`
    6. Kill switch does NOT suppress decrement — accounting must stay correct regardless
  - **Orphan TTL (triggered on each WorkloadProfile reconcile):**
    1. If `Orphaned` condition is true and `now - lastTransitionTime >= BallastConfig.orphanTTL`
    2. Calls `store.AllKeysForHash` and `store.DeleteKey` to purge Redis data
    3. Deletes the `WorkloadProfile` object
  - envtest-based tests covering: create, increment, decrement, orphan transition, orphan TTL deletion, Redis key cleanup, kill switch suppression

### Key files

- `internal/controller/workloadwatcher/controller.go` — `PodReconciler` (pod CREATE/DELETE) and `ProfileReconciler` (orphan TTL cleanup); exported `ExtractTupleLabels` and `ProfileName` used by webhook
- `internal/controller/workloadwatcher/controller_test.go`

### User testing instructions

_Deploy to a test cluster with a pod carrying the `measure` annotation. Confirm `WorkloadProfile` is created. Delete the pod; confirm `activeWorkloads` decrements and `Orphaned` is set._

---

## Phase 8 — MetricsCollector Controller

**Status:** `[x]`
**Depends on:** Phases 3, 4, 5, 6, 7
**PR:** https://github.com/Tight-Line/ballast/pull/11

### What to build

- `internal/controller/metricscollector/controller.go`:
  - Reconciles `WorkloadProfile` objects on a timer (interval from matched policy's `MetricsSource.spec.config.pollInterval`; uses `ctrl.Result{RequeueAfter: interval}`)
  - For each profile (refined in PR #21 — no longer loads BallastConfig; uses list-based store API):
    1. Guard: if `status.selectorLabels == nil`, requeue after 5s (workloadwatcher hasn't written them yet)
    2. Find matching policy (via `policy.Resolver`)
    3. Resolve `MetricsSource` from policy; look up plugin from registry
    4. Call `plugin.FetchStats(ctx, identity, TimeWindow{End: now})` — no start boundary; reservoir cap limits history
    5. For each `ContainerStats` returned: call `store.AddSample` (RPUSH+LTRIM+SET NX first_seen)
    6. Query Redis: `store.QueryAll` for all stored values; `store.FirstSeenMs` for wall-clock span; compute stats via `store.ComputeStats`
    7. Evaluate readiness: `sampleCount >= minDataPoints`, `timeSpan >= minTimeSpan`, `CV <= maxCV` — all must pass
    8. If ready: compute recommendations for each (resource, field) pair: `aggregatedValue * headroom`
    9. Update `WorkloadProfile` status: `usageStats`, `recommendations`, `meetsThreshold`, conditions
  - **Dry-run (`--dry-run-measure`):** steps 4 and 8 are skipped; log at `info` with `dry_run: true` what would have been written
  - **Kill switch:** steps 4 and 8 are skipped; log at `warn` with `kill_switch: true`
- `internal/stats/aggregator.go`:
  - `EvaluateReadiness(stats Stats, readiness ReadinessConfig) bool`
  - `ComputeRecommendation(stats Stats, metric MetricConfig) resource.Quantity`
- envtest + miniredis tests

### Key files

- `internal/controller/metricscollector/controller.go` — `Reconciler`; `Setup(mgr, ks, sc, dryRunMeasure)` entry point; full poll-write-query-evaluate-update cycle
- `internal/controller/metricscollector/controller_test.go`
- `internal/stats/aggregator.go` — `EvaluateReadiness(Stats, firstMs, lastMs, ReadinessConfig) bool`; `ComputeRecommendation(Stats, MetricConfig) (resource.Quantity, error)`
- `internal/stats/aggregator_test.go`

### User testing instructions

_Deploy to test cluster. After one `pollInterval`, confirm `WorkloadProfile` status shows populated `usageStats`. After `minDataPoints` samples, confirm `meetsThreshold: true` and `recommendations` are populated._

---

## Phase 9 — Admission Webhook

**Status:** `[x]`
**Depends on:** Phases 3, 4, 7, 8
**PR:** https://github.com/Tight-Line/ballast/pull/12

### What to build

- `internal/webhook/pod_mutator.go` — implements `admission.Handler` for pod CREATE:
  1. **Kill switch:** if active, return allow without mutation; log `warn`
  2. **Annotation validation:** call `validation.ValidateAnnotations`; if invalid, return deny with descriptive message
  3. **Resolve progressive mode:** if `autoresize` annotation set, read `WorkloadProfile.meetsThreshold`; if false, treat as `measure`-only; if true, treat as `apply+resize`
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
  - autoresize: before threshold (measure-only), after threshold (apply + resize)

### Key files

- `internal/webhook/pod_mutator.go` — `PodMutator` admission handler; `Handle`, `resolveApplyProfile`, `mutate`, `stampPolicyRef`, `lookupProfile`, `applyRecommendations`
- `internal/webhook/pod_mutator_test.go` — 19 unit tests + envtest `SetupWithManager` test
- `cmd/ballastd/main.go` — `--dry-run-apply` flag; `NewPodMutator(...).SetupWithManager(mgr)` registration
- `internal/controller/workloadwatcher/controller.go` — exported `ProfileName` and `ExtractTupleLabels` for webhook use

### User testing instructions

_Deploy to test cluster with cert-manager. Create a pod with `ballast.tightlinesoftware.com/apply: "true"`. Confirm resources are patched (or not, if profile not ready). Try an invalid annotation combo; confirm rejection with clear message._

---

## Phase 10 — ResourceAdjuster Controller

**Status:** `[x]`
**Depends on:** Phases 3, 4, 7, 8, 9
**PR:** https://github.com/Tight-Line/ballast/pull/13

### What to build

Pod eviction is out of scope for Ballast — delegated to [Kubernetes Descheduler](https://github.com/kubernetes-sigs/descheduler). Ballast keeps resource requests/limits accurate; Descheduler rebalances the cluster based on those corrected values. See README for rationale.

- `internal/controller/resourceadjuster/controller.go`:
  - Watches `WorkloadProfile` status changes (triggered when MetricsCollector updates status)
  - Re-evaluates on `behaviors.resize.interval` timer
  - **Drift detection:** for each container/resource/field in `recommendations`, compare current pod value to recommended value; compute drift as `|current - recommended| / recommended`; look up threshold via coalesce order (`resourceThresholds -> resize.default -> thresholds.default`); trigger if drift exceeds threshold for any field
  - **Resize path (if `resize` annotation present and drift exceeds threshold):**
    1. Cap adjustment to `maxChangePerCycle` relative to the current→recommended gap
    2. Patch pod via `resize` subresource (`v1.Pod` resize API, Kubernetes 1.35+)
    3. On success: record in pod annotation and profile status
    4. On failure (node pressure / infeasible): emit Kubernetes Event, record blocked state in profile status, requeueAfter interval
  - **`autoresize`:** once `WorkloadProfile.meetsThreshold` transitions from false to true, the controller dynamically enables resize per reconcile
  - **Dry-run (`--dry-run-resize`):** log the resize; pod not touched
  - **Kill switch:** suppresses all action
  - envtest tests: drift detection, resize success, resize blocked, autoresize threshold flip, dry-run, kill switch

### Key files

- `internal/controller/resourceadjuster/controller.go` — `Reconciler`; `Setup(mgr, ks, dryRunResize)` entry point; exported utilities: `ExceedsDrift`, `CapChange`, `ResolveFieldThreshold`, `ParseResizeInterval`
- `internal/controller/resourceadjuster/controller_test.go`

### User testing instructions

_Deploy to test cluster with a pod carrying `resize` annotation and a ready profile. Artificially update the profile's recommendations to force drift beyond threshold. Confirm resize attempt is made via the pod resize subresource._

---

## Phase 11 — Helm Chart

**Status:** `[x]`
**Depends on:** all prior phases (chart reflects final binary)
**PR:** https://github.com/Tight-Line/ballast/pull/16

### What to build

`charts/ballast/` containing:

- `Chart.yaml` — `apiVersion: v2`, dependencies include `bitnami/valkey` (with `condition: valkey.enabled`)
- `values.yaml` — all configurable settings with defaults matching DESIGN.md:
  - `image.repository`, `image.tag`, `image.pullPolicy`
  - `replicaCount: 1`
  - `logging.level`, `logging.webhook`, `logging.watcher`, `logging.collector`, `logging.adjuster`, `logging.format`
  - `dryRun.measure`, `dryRun.apply`, `dryRun.resize` (all false)
  - `ballastConfig.identityLabels`, `ballastConfig.orphanTTL`, `ballastConfig.retentionWindow`
  - `valkey.enabled: true`, `valkey.architecture: replication`
  - `store.endpoint` (used when `valkey.enabled: false`)
  - `certManager.enabled: true` (see TLS note below)
- `templates/deployment.yaml` — mounts cert Secret; passes all flags from values
- `templates/serviceaccount.yaml`, `templates/clusterrole.yaml`, `templates/clusterrolebinding.yaml` — exact permissions: CRD read/write for all Ballast types, Pod get/list/watch/update/patch + pods/resize subresource patch (for finalizer management and in-place resize), ConfigMap get/watch (kill switch), Event create
- `templates/mutatingwebhookconfiguration.yaml` — `failurePolicy: Fail`; cert-manager `caBundle` injection annotation (`cert-manager.io/inject-ca-from`)
- `templates/certificate.yaml`, `templates/issuer.yaml` — self-signed `Issuer` + `Certificate`; rendered when `certManager.enabled: true`
- `templates/ballastconfig.yaml` — creates the `BallastConfig` singleton from values
- `crds/` — CRD manifests (copied from `config/crd/bases/` at build time)

**TLS options — implement option 1 now; 2 and 3 are future improvements:**

1. **cert-manager (default):** self-signed `Issuer` + `Certificate`; cert-manager injects `caBundle` into `MutatingWebhookConfiguration`. Uses whatever cert-manager is already installed in the cluster — Ballast does not install it. Works on air-gapped clusters; no DNS/HTTP challenge.
2. **Kubernetes CertificateSigningRequest:** Helm pre-install Job generates a key pair, submits a CSR to the cluster CA, writes cert+key to a Secret. Removes the cert-manager dependency but requires `certificates.k8s.io/approve` permission; may need manual `kubectl certificate approve` on restricted clusters.
3. **User-provided certificate:** user supplies cert material via Helm values; chart skips cert-manager and CSR resources entirely. User is responsible for setting `caBundle` in the `MutatingWebhookConfiguration`.

`helm lint` must pass. Smoke test: `helm template . | kubectl apply --dry-run=client -f -` produces no errors.

### Key files

- `charts/ballast/Chart.yaml` — chart metadata; `bitnami/valkey` dependency with `condition: valkey.enabled`
- `charts/ballast/values.yaml` — all configurable settings: image, logging levels, dry-run flags, `ballastConfig.*`, `valkey.*`, `store.endpoint`, `certManager.enabled`
- `charts/ballast/templates/deployment.yaml` — operator deployment; mounts cert Secret; translates all values to CLI flags
- `charts/ballast/templates/clusterrole.yaml` — exact RBAC: all Ballast CRDs, pod patch, ConfigMap watch, Event create
- `charts/ballast/templates/mutatingwebhookconfiguration.yaml` — `failurePolicy: Fail`; cert-manager `caBundle` injection annotation
- `charts/ballast/templates/ballastconfig.yaml` — creates the `BallastConfig` singleton from values
- `charts/ballast/templates/metricssource.yaml` — creates the default `kubernetes-metrics` MetricsSource (opt-out via `defaultMetricsSource.enabled: false`)
- `charts/ballast/templates/clusterresourcepolicy.yaml` — creates the default `default` ClusterResourcePolicy (opt-out via `defaultClusterResourcePolicy.enabled: false`)
- `charts/ballast/crds/` — bundled CRD manifests (copied from `config/crd/bases/` at release)

### User testing instructions

_Install chart into a kind cluster with cert-manager pre-installed. Confirm operator pod starts healthy. Confirm `BallastConfig` singleton exists. Create a test pod with `measure` annotation; confirm `WorkloadProfile` is created._

---

## Phase 12 — Polish & Release Readiness

**Status:** `[x]`
**Depends on:** all prior phases
**PR:** https://github.com/Tight-Line/ballast/pull/17

### What to build

- `AGENTS.md` complete: all file locations, key functions, build commands, coding standards, testing workflow, PR image workflow, release workflow, provider development guide
- `README.md`: prerequisites (cert-manager, Kubernetes 1.35+, metrics-server), Helm quickstart, first annotation walkthrough, verifying a `WorkloadProfile`, kill switch usage
- `CHANGELOG.md` `[Unreleased]` section fully populated
- `.github/workflows/release.yml`: triggers on `v*` tag push; builds and pushes `ghcr.io/tight-line/ballast:<tag>` + `:latest` (multi-arch); packages `ballast-<ver>.tgz` via chart-releaser and publishes to `gh-pages` Helm repo
- Verify `.github/workflows/pr-images.yml` produces correct image tags end-to-end
- `make check` clean on main branch
- This plan updated with all key-file entries filled in

### Key files

- `AGENTS.md` — complete agent guide: file index, architecture, CRD types, controller descriptions, plugin interface, Redis data model, testing strategy, build/CI, coding standards, PR image + release workflow, provider development guide
- `README.md` — project overview, prerequisites, installation, WorkloadProfile identity, annotation contract, verifying a WorkloadProfile, kill switch, webhook TLS, dry-run mode, development workflow
- `CHANGELOG.md` — `[Unreleased]` section populated with all features from phases 1–11
- `.github/workflows/release.yml` — tag-triggered release: multi-arch container image + Helm chart tarball via chart-releaser
- `IMPLEMENTATION_PLAN.md` — this file; all key-file entries filled in; Phase 10 status corrected to `[x]`

### User testing instructions

_Follow the README quickstart against a real cluster from scratch. Confirm every step works as written._

---

## Phase 13 — Prometheus and OpenTelemetry Metrics

**Status:** `[x]`
**Depends on:** Phase 12

### What to build

- `internal/metrics/` package: `MeterProvider` that can drive a Prometheus exporter on the existing `/metrics` endpoint, push to an OTLP collector, or both
- Instrument all five components (MetricsCollector, WorkloadWatcher, ResourceAdjuster, Admission Webhook, KillSwitch) with OTel counters and a kill-switch gauge
- New `ballastd` flags: `--otel-metrics-endpoint`, `--otel-metrics-protocol`, `--otel-metrics-interval`, `--otel-metrics-insecure`
- Prometheus enabled automatically when `--metrics-bind-address` is not `"0"`
- OTel service resource attributes (`service.name`, `service.version`, `service.namespace`) with hardcoded defaults overridable via `OTEL_SERVICE_NAME` / `OTEL_RESOURCE_ATTRIBUTES`
- Helm chart `telemetry:` section: toggle Prometheus (with optional `ServiceMonitor`), OTLP endpoint, `serviceName`, `serviceNamespace`
- Refactor `main()` into `run() int` so deferred `shutdownMetrics` executes before `os.Exit`

### Key files

- `internal/metrics/provider.go` — `MeterProvider` setup: Prometheus exporter, OTLP exporter, OTel Resource attributes
- `internal/metrics/recorder.go` — per-component OTel counter and gauge definitions
- `internal/metrics/metrics_test.go` — full coverage of provider and recorder
- `cmd/ballastd/main.go` — new flags wired to `SetupProvider`; `run()` refactor
- `charts/ballast/templates/metrics-service.yaml` — `Service` exposing `/metrics` for scraping
- `charts/ballast/templates/servicemonitor.yaml` — optional `ServiceMonitor` for Prometheus Operator
- `charts/ballast/values.yaml` — `telemetry:` stanza

### User testing instructions

_Deploy with `--metrics-bind-address=:8080` and `kubectl port-forward`; confirm `/metrics` returns ballast counters. Configure a `ServiceMonitor` and verify Prometheus picks up the target._

---

## Phase 14 — `kubeletSummary` Plugin (Ephemeral Storage Metrics)

**Status:** `[x]`
**Depends on:** Phase 6

### What to build

- New plugin package `internal/plugin/kubelet/` implementing `plugin.MetricsPlugin` with type name `kubeletSummary`
- Data source: kubelet Summary API via the Kubernetes API server proxy (`GET /api/v1/nodes/{nodeName}/proxy/stats/summary`), accessed with the controller-runtime rest.Config so no extra credentials are needed
- Per-node fan-out: list all nodes, fetch each node's summary in parallel, aggregate pod entries cluster-wide
- Extract `ephemeral-storage.usedBytes` per pod and map to `ContainerStats` entries keyed by `podRef.name/namespace`; associate with containers by matching pod identity (the summary API reports per-pod, not per-container — model as the first/only container or distribute evenly if multiple containers are present; document the limitation)
- Client-side label filtering using the same `podMatchesSelector` logic as the `kubernetesMetrics` plugin
- Per-node TTL cache (default 55 s) with stale-data detection: if a node's cached entry is older than `2 * CacheTTL`, emit a warning log and skip that node's data rather than returning stale values
- Per-workload exponential backoff on node errors, same pattern as `kubernetesMetrics`
- Register the plugin in `cmd/ballastd/main.go` using the rest.Config from the controller-runtime manager
- Add `nodes/proxy` RBAC rule (`get` verb) to the Helm chart ClusterRole
- Add a `defaultKubeletSummaryMetricsSource` section in `values.yaml` (enabled by default) that installs a `MetricsSource` object named `kubelet-summary` with type `kubeletSummary`

### Key files

- `internal/plugin/kubelet/plugin.go` — plugin implementation: node listing, per-node fetch, cache, backoff, label filter
- `internal/plugin/kubelet/plugin_test.go` — unit tests with a fake node lister and summary API
- `cmd/ballastd/main.go` — register `kubelet.New(restConfig, kubelet.DefaultOptions())`
- `charts/ballast/templates/clusterrole.yaml` — add `nodes/proxy` get rule
- `charts/ballast/templates/metricsource-kubelet.yaml` — new `MetricsSource` template
- `charts/ballast/values.yaml` — `defaultKubeletSummaryMetricsSource` stanza

### User testing instructions

_After deploying, confirm the `kubelet-summary` MetricsSource appears and that `WorkloadProfile.status.containers[*]` begins populating `ephemeral-storage` stats after one collection cycle. Check controller logs for per-node fetch success/failure._

---

## Phase 15 — Updated Default Policy, Memory Limits, and Testing Policy

**Status:** `[x]` — Complete (PR pending)
**Depends on:** Phase 14, Phase 11

### What to build

**Default policy formula change (CPU and memory requests):**

Replace the current `p95 * headroom` approach with `avg * 1.25` (= mean / 0.80 target utilization). For a large homogeneous fleet the aggregate memory pressure is predictable; sizing at 80% of mean lets nodes run dense while leaving headroom for normal variation. The 20% drift threshold means the adjuster only fires when actual usage has moved by more than 20% relative to the recommendation, avoiding churn.

**Sampling cadence and readiness (implemented):** the default `kubernetesMetrics`/`kubeletSummary` poll interval moved from 60s to 5m (production fleets don't need second-resolution sampling), and `readiness.minDataPoints` dropped from 500 to 250 so a single long-running pod accrues enough samples (~288 over 24h at the 5m cadence) to cross the gate at the 24h `minTimeSpan` mark rather than being blocked on sample count.

**New aggregations (implemented):** the ephemeral-storage request needs `p90`, which the stats engine did not compute and the `MetricConfig.aggregation` enum did not allow. Added `p75` and `p90` end to end: `store.Stats` + `ComputeStats` (`internal/store/percentiles.go`), the `ContainerUsageStats` CRD fields (`api/v1/workloadprofile_types.go`) populated by the collector, the `aggregation` enum (`api/v1/clusterresourcepolicy_types.go`), and the resolver switch (`internal/stats/aggregator.go`).

**Memory limit (new):**

Add a memory limit entry: `aggregation: p99, headroom: "1.0"`. p99 is the highest usage this workload has shown in production; pods that exceed it are likely leaking or misbehaving and should be OOMKilled. This gives Burstable QoS (limit > request), which is the correct class for most production workloads. CPU limits are intentionally not added.

**Ephemeral storage (new, requires Phase 14):**

Add two ephemeral storage entries pointing at the `kubelet-summary` MetricsSource:
- Request: `aggregation: p90, headroom: "1.0"` — sizes for the growth-skewed distribution
- Limit: `aggregation: p99, headroom: "1.0"` — triggers eviction before disk pressure hits the node

**Template fix (per-metric source):**

The current `clusterresourcepolicy.yaml` template uses a single global `metricsSource` for all metric entries. Update it to prefer `.source` from the metric entry if present, falling back to the global default: `{{ .source | default $.Values.defaultClusterResourcePolicy.metricsSource | quote }}`. This lets CPU/memory entries reference `kubernetes-metrics` and ephemeral-storage entries reference `kubelet-summary` within the same policy object.

**Policy presets (implemented; supersedes the original `testClusterResourcePolicy` design):**

Rather than embedding a dev-only `testClusterResourcePolicy` (stanza + always-present template gated by `enabled`) inside the production chart, ship a catalog of policy presets as Helm values overlays under `charts/ballast/presets/`. Anything in the chart is published in every release artifact, so a dev-only resource baked in (even gated) is a footgun; an overlay is inert data that only takes effect when selected with `-f`. The base `values.yaml` default IS the `homogeneous-large-fleet` preset, so a plain `helm install` is production-sane with no flags.

- `charts/ballast/presets/local-testing.yaml` — fast-cycle overlay for kind clusters: `readiness.minDataPoints: 5`, `minTimeSpan: "1m"`, `maxCV: "2.0"`, `behaviors.resize.interval: "30s"`, poll intervals overridden to 15s, and a CPU/memory-only metrics list (drops the ephemeral-storage entries to keep the local loop focused). A prominent header comment states it is development-only.
- `charts/ballast/presets/README.md` — documents the catalog and how to apply/override presets.

Helm's native `-f` merge gives the preset + override behavior for free (map fields deep-merge, list fields replace), so no template logic and no extra `priority` arbitration is needed — only one `ClusterResourcePolicy` is ever rendered.

**`make helm-update-local` change:**

`helm-update-local` sets a target-specific `HELM_LOCAL_EXTRA = -f $(CHART_DIR)/presets/local-testing.yaml` (which GNU Make propagates to its `helm-install-local` prerequisite) so local kind clusters always get the fast-cycle preset without manual flags.

**Testing instructions (add to README or a dedicated TESTING.md):**

Document the full local test loop:
1. `make helm-update-local KIND_CLUSTER=<name>` — deploys ballast with the test policy active
2. Install nginx with autoresize: `helm install nginx bitnami/nginx --set commonAnnotations."ballast\.tightlinesoftware\.com/autoresize"=true`
3. `kubectl get -w workloadprofile nginx--nocomponent` — watch `meetsThreshold` flip to true after ~5 samples (~1 min)
4. `kubectl get -w pods -n nginx` — observe admission apply resources on the next pod creation; watch the ResourceAdjuster fire at the 30 s interval if the OOTB requests (50m CPU, generous memory) differ from the profile recommendation by >20%
5. `kubectl describe pod <name> -n nginx` — confirm `last-resize` annotation is stamped and container resources match profile recommendations

### Key files

- `internal/store/percentiles.go`, `api/v1/workloadprofile_types.go`, `internal/controller/metricscollector/controller.go`, `api/v1/clusterresourcepolicy_types.go`, `internal/stats/aggregator.go` — add `p75`/`p90` aggregations end to end (plus regenerated CRDs synced to `charts/ballast/crds/`)
- `charts/ballast/values.yaml` — `defaultClusterResourcePolicy.metrics` (avg*1.25 requests + p99 memory limit + p90/p99 ephemeral-storage entries via `kubelet-summary`); 5m poll intervals; `minDataPoints: 250`
- `charts/ballast/templates/clusterresourcepolicy.yaml` — per-metric `source` fallback (`.source | default $.Values...metricsSource`)
- `charts/ballast/presets/local-testing.yaml`, `charts/ballast/presets/README.md` — fast-cycle preset overlay and catalog docs
- `Makefile` — `helm-update-local` applies the local-testing preset via target-specific `HELM_LOCAL_EXTRA`
- `TESTING.md` — local test loop instructions (linked from `README.md`)

---

## Dependency Graph

```
Phase 1 (scaffold)
  └─ Phase 2 (CRD types)
       ├─ Phase 3 (logger + kill switch)
       ├─ Phase 4 (policy resolution)
       └─ Phase 5 (Redis/Valkey layer)
            └─ Phase 6 (plugin interface + kubernetesMetrics)
                 ├─ Phase 14 (kubeletSummary plugin)
                 └─ Phase 7 (WorkloadWatcher) ← also needs 3, 4, 5
                      └─ Phase 8 (MetricsCollector) ← also needs 3, 4, 5, 6
                           └─ Phase 9 (Admission webhook) ← also needs 3, 4, 7
                                └─ Phase 10 (ResourceAdjuster) ← also needs 3, 4, 7, 8, 9
                                     └─ Phase 11 (Helm chart)
                                          ├─ Phase 12 (polish)
                                          └─ Phase 15 (default/test policy) ← also needs 14
```

Phases 3, 4, and 5 can run in parallel after Phase 2 is complete. Phases 6 and 7 can start once their dependencies are done; they are independent of each other. Phase 14 depends only on Phase 6 and can be implemented independently of Phases 7-13. Phase 15 depends on both Phase 11 (Helm chart structure) and Phase 14 (ephemeral storage data).
