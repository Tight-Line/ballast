# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Helm chart now installs a default `MetricsSource` (`kubernetes-metrics`) wired to the built-in Kubernetes Metrics API and a default `ClusterResourcePolicy` (`default`) with conservative CPU/memory request sizing on first install. Both can be disabled with `defaultMetricsSource.enabled: false` / `defaultClusterResourcePolicy.enabled: false` in values.
- Local kind cluster development workflow: `make docker-kind KIND_CLUSTER=<name>` (build image for host arch + load into kind), `make helm-install-local` (install chart with local image), `make helm-update-local KIND_CLUSTER=<name>` (combined one-shot rebuild and redeploy). Host CPU architecture is detected automatically via `uname -m`.

### Fixed

- `ClusterRole` was missing the `update` verb on `pods` (required for adding/removing the workloadwatcher finalizer) and had no rule for the `pods/resize` subresource (required for in-place resize). Both omissions caused `403 Forbidden` errors at runtime.
- `activeWorkloads` on `WorkloadProfile` could be over-counted when a transient error caused the workloadwatcher to retry pod processing. The counter was incremented before writing the pod finalizer; each failed `Update` retry incremented it again. The counter is now incremented only after the finalizer write succeeds, making it idempotent with respect to retries.

## [0.1.3] - 2026-06-28

### Fixed

- Pods opted into ballast (via annotation) that are missing one or more identity labels are no longer silently skipped. Absent label keys now contribute a human-readable placeholder to the WorkloadProfile name (e.g. `app.kubernetes.io/component` absent → `nocomponent`), so the profile is still created and measurements proceed.

## [0.1.2] - 2026-06-28

- Fix app version tagging so that Helm chart defaults match actual git tags and container image tags

## [0.1.1] - 2026-06-28

### Changed

- Release workflow: build and push amd64 image first for fast availability, then follow with multi-arch (amd64 + arm64) manifest
- Release workflow: add `helm repo add` step so chart dependency update succeeds in CI
- Helm install instructions: use `ballast` as the repo alias (`helm repo add ballast ...`) instead of `tight-line`

## [0.1.0] - 2026-06-28

### Added

**Operator core**

- `ballastd` binary: kubebuilder-managed operator with leader election, health/readiness probes, and structured logging
- Per-component log levels (`--log-level`, `--log-level-webhook`, `--log-level-watcher`, `--log-level-collector`, `--log-level-adjuster`) and format selection (`--log-format json|text`)
- Kill switch: creating the `ballast-kill-switch` ConfigMap in the operator namespace halts all mutation and resize activity without a restart; `BallastConfig.spec.suspended: true` provides the same effect in a GitOps-friendly way
- Independent dry-run flags: `--dry-run-measure`, `--dry-run-apply`, `--dry-run-resize`; each logs what would have happened at `info` level with `dry_run: true`

**CRD types**

- `BallastConfig` (cluster-scoped singleton): `identityLabels`, `orphanTTL`, `retentionWindow`, `suspended`
- `MetricsSource` (cluster-scoped): names a metrics plugin type and its poll configuration (`pollInterval`, `reservoirSize`)
- `ClusterResourcePolicy` (cluster-scoped): full selector (kinds, namespace include/exclude regex, annotation patterns, `labelSelector`), metrics slice, readiness thresholds, and resize behaviors
- `ResourcePolicy` (namespace-scoped): identical spec to `ClusterResourcePolicy`; always overrides any `ClusterResourcePolicy` match
- `WorkloadProfile` (cluster-scoped, status subresource): accumulates per-container usage statistics and resource recommendations; tracks `meetsThreshold`, `activeWorkloads`, and Ready/Orphaned conditions

**Workload identity and annotation contract**

- Identity tuple: pods are grouped into `WorkloadProfile` objects by a configurable set of label keys (`BallastConfig.spec.identityLabels`); the profile name is derived from the sorted label values
- Opt-in annotations: `measure`, `apply`, `resize`, `autoresize` under the `ballast.tightlinesoftware.com/` prefix
- `autoresize` progressive mode: starts in measure-only mode, then automatically activates apply + resize once the readiness threshold is met
- Annotation combination validation enforced at admission time with descriptive rejection messages

**WorkloadWatcher controller**

- Creates `WorkloadProfile` objects when the first opted-in pod is seen; sets `tupleLabels` in status
- Stamps `ballast.tightlinesoftware.com/profile-ref` on pods for downstream controllers
- Tracks `activeWorkloads`; sets `Orphaned` condition when count reaches zero
- Orphan TTL: purges all Redis time-series keys and deletes the `WorkloadProfile` after the configured `orphanTTL` elapses

**MetricsCollector controller**

- Polls the configured metrics plugin on a timer derived from the matched policy's `MetricsSource.pollInterval`
- Writes per-container CPU and memory samples into Redis sorted sets keyed by identity tuple hash, container name, and resource
- Enforces retention window (`BallastConfig.spec.retentionWindow`) and per-key reservoir cap (`MetricsSource.spec.config.reservoirSize`)
- Computes p50/p95/p99/max/mean/stddev/CV from the accumulated sample window
- Evaluates readiness: `minDataPoints`, `minTimeSpan`, and `maxCV` must all pass
- Writes computed statistics and resource recommendations to `WorkloadProfile` status

**Admission webhook**

- Mutating webhook for pod CREATE; registered at `/mutate-v1-pod`; `failurePolicy: Fail`
- Patches container `resources.requests` and `resources.limits` at admission time when the matching `WorkloadProfile` is ready
- Stamps `ballast.tightlinesoftware.com/policy-ref` on admitted pods
- Progressive `autoresize` resolution: reads `WorkloadProfile.meetsThreshold` at admission time to decide whether to apply

**ResourceAdjuster controller**

- Watches `WorkloadProfile` status changes and runs on a periodic requeue timer (default 15 minutes)
- Computes drift between current container resource values and profile recommendations
- Threshold lookup follows a coalesce chain: per-resource field override → resize default → global default (20%)
- Bounds each adjustment to `maxChangePerCycle` (default 50%) per cycle to avoid sudden large changes
- Issues in-place pod resize patches via the Kubernetes resize subresource (requires Kubernetes 1.35+)
- On infeasible resize (node pressure): emits a Kubernetes Event and stamps `ballast.tightlinesoftware.com/resize-blocked` annotation; retries on next cycle

**Metrics plugin system**

- `MetricsPlugin` interface with global registry; plugins self-register via `init()`
- Built-in `kubernetesMetrics` plugin: calls the in-cluster metrics API (`metrics.k8s.io/v1beta1`); returns current CPU, memory, and ephemeral-storage usage for all regular containers
- Token-bucket rate limiter (configurable RPS) shared across concurrent polls
- Exponential backoff (base 1s, configurable ceiling, default 5 minutes) on API errors; caller skips the cycle while in backoff

**Redis/Valkey storage layer**

- `Client` interface over go-redis/v9; tests use miniredis for isolation
- Deterministic `TupleHash` (SHA-256 prefix of sorted `key=value` pairs) for stable, map-order-independent keys
- Sorted-set time-series operations: `AddSample`, `QueryWindow`, `ExpireOlderThan`, `EnforceReservoirCap`, `SampleCount`, `TimeRange`, `DeleteKey`
- `AllKeysForHash` for full-profile Redis cleanup on orphan deletion

**Policy resolution**

- `Resolver` evaluates all `ClusterResourcePolicy` and `ResourcePolicy` objects from the controller-runtime cache
- Namespace selectors support both exact-match strings and `/regex/` patterns; include and exclude lists; both-match = excluded with a `warn` log
- Annotation selectors support the same exact-match and `/regex/` syntax
- Standard `metav1.LabelSelector` evaluation via `k8s.io/apimachinery/pkg/labels`
- Precedence: `ResourcePolicy` beats `ClusterResourcePolicy`; within the same class, higher `priority` wins; ties break alphabetically

**Helm chart**

- `charts/ballast/` chart with `bitnami/valkey` as an optional dependency (`valkey.enabled: true` by default)
- cert-manager TLS integration: self-signed `Issuer` + `Certificate`; cert-manager injects `caBundle` into `MutatingWebhookConfiguration` automatically
- Full Helm values for logging levels, dry-run flags, `BallastConfig` parameters, Redis endpoint, and image coordinates
- RBAC: `ClusterRole` with exact permissions for all Ballast CRDs, pod get/list/watch/patch (resize), ConfigMap get/watch (kill switch), and Event create
- CRD manifests bundled in `charts/ballast/crds/` for automatic install/upgrade

**CI/CD**

- `ci.yml`: parallel test / lint / build on every PR and main push; Codecov upload
- `pr-images.yml`: builds `ghcr.io/tight-line/ballast:pr-<n>-<sha>` on every PR push; posts image tag and Helm override to the PR as a comment
- `snyk.yml`: dependency vulnerability scan (high+ severity) on PRs, main, and weekly
- `sonar.yml`: SonarCloud static analysis on PRs and main
- Pre-commit hook (`scripts/pre-commit`): `goimports` format check + golangci-lint
