# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **Helm `NOTES.txt`: enroll a workload with the `mode` label, not the removed `enroll` annotation.** The post-install "NEXT STEP" instructions still told users to add `ballast.tightlinesoftware.com/enroll: measure`, an annotation the `0.4.0` enrollment-API change replaced. They now say to add the `ballast.tightlinesoftware.com/mode: measure` label, matching the README and `scripts/enroll.sh`.

- **`scripts/enroll.sh`: the no-restart route left Deployments half-enrolled and could not repair them.** Three related defects, each of which produced a pod carrying the workloadwatcher finalizer but no `mode` label — the exact state the label-scoped informer cache cannot see, so on deletion the pod wedges in `Terminating` forever:
  - The post-resume guard that flagged "controller promoted or created a different ReplicaSet" keyed on ReplicaSet identity and count. A Deployment template edit legitimately bumps the revision and churns the owned-ReplicaSet set (history GC) **without restarting any pod**, so the guard fired false positives; worse, it `return`ed before the live pods were labeled, leaving the template enrolled but the running pod not. The guard is gone; the reliable "did anything restart" signal is now the running-pod UID diff in `verify_owner`, which also no longer counts a pre-existing terminating pod (a finalizer-wedged ghost) as a failure.
  - Re-running could not fix a half-enrolled Deployment: candidates were selected on the template label alone, so a Deployment whose template already carried the label (but whose ReplicaSet or pods did not) was treated as "already enrolled" and skipped. Deployments are now selected on the identity tuple alone and `process_deployment` examines the current ReplicaSet and the pods, repairing a half-enrolled state and reporting a genuinely converged one as a quiet no-op.
  - Pod labeling covered only running, non-terminating pods. It now labels **every** pod the selector matches in any phase — running, pending, succeeded (`Completed`), failed (`Error`), and terminating — which both completes enrollment and lets the operator reap a finalizer-wedged terminating pod once it becomes cache-visible. Pods already at the mode are skipped, so re-runs stay idempotent.

## [0.4.2] - 2026-07-20

### Added

- **`scripts/enroll.sh`: bulk opt-in helper.** Adds the `ballast.tightlinesoftware.com/mode` label (`measure`, `apply`, or `resize`, chosen with the required `--mode`) to every Deployment/StatefulSet/DaemonSet whose pod template already carries the full identity tuple and is not yet enrolled. The tuple is resolved from the live `BallastConfig` named `ballast` in the target context (`.spec.identityLabels`, the same tuple the operator keys on), overridable with `--identity-labels`, and falls back to the chart default (`app.kubernetes.io/name` + `app.kubernetes.io/component`). Multi-replica workloads are rolling-restarted so their pods pick the label up; single-replica and `OnDelete` workloads are enrolled without any restart by patching the template durably and labeling the live pods in place (the `mode` label is not in any selector, so this never triggers a restart). `--no-restart` forces the no-restart route for every workload (a fast in-place first enrollment; at `--mode apply` in-place pods keep their resources until they next restart, while `measure`/`resize` are unaffected), and `--remode` changes workloads already enrolled at a different mode to `--mode` instead of leaving them alone (a fast mode-upgrade, in place when combined with `--no-restart`). Dry-run by default (`--apply` to execute), with `-n/--namespace`, `--context`, `--ignore` (workload-name regex, default none — enrolls everything; the no-restart route makes a full sweep safe), `--timeout`, and `--settle`. Documented under "Bulk enrollment" in the README.

## [0.4.1] - 2026-07-20

### Added

- **Helm: `tolerations` support for the operator and Valkey pods.** A new top-level `tolerations` value is applied to both the operator Deployment and the pre-install/pre-upgrade `crd-upgrade-hook` Job (a taint that keeps Ballast off a node should keep its bootstrap Job off it too, otherwise the hook fails to schedule and blocks the install/upgrade before the operator starts). The Valkey store's tolerations are set via `valkey.tolerations`, which passes straight through to the upstream subchart. All default to empty, so this is additive. ([#62](https://github.com/Tight-Line/ballast/issues/62))

## [0.4.0] - 2026-07-20

> ⚠️ **BREAKING RELEASE.** This release changes the enrollment API. **Every enrolled
> workload must be re-labeled or Ballast silently stops managing it.** Read the
> Changed entry below and migrate before (or at) upgrade. Per the pre-v1 rule this
> is a **minor** version bump (`0.3.x` → `0.4.0`).

### Changed

- **BREAKING: enrollment moved from four annotations to a single `ballast.tightlinesoftware.com/mode` label.** Workloads used to opt in with `ballast.tightlinesoftware.com/measure`, `/apply`, `/resize`, and `/autoresize` annotations. They now set one label, `ballast.tightlinesoftware.com/mode`, whose value names a rung on an escalating ladder: `measure` (collect only), `apply` (measure + admission-time patching), or `resize` (apply + in-place resize). Each rung implies the ones below it, so `resize` is exactly what `autoresize` used to mean; the `autoresize` name is gone, and there is no longer a resize-without-apply combination (there was never a use for one). A pod carrying the label with any other value is rejected by the webhook.

  The driver is scale. Annotations cannot be used in label selectors, so the operator's pod informer and the admission webhook previously had to consider every pod in the cluster and filter in-process. With enrollment as a label, the API server filters server-side: the manager's pod cache is scoped to enrolled pods, and the `MutatingWebhookConfiguration` carries an `objectSelector` so the API server never calls the webhook for unenrolled pods. On a large cluster (the motivating case was ~100k pods) Ballast's footprint is now proportional to the number of enrolled pods rather than the total pod count. ([#55](https://github.com/Tight-Line/ballast/issues/55))

  Ballast's own outputs are unchanged and remain annotations, because they carry values a label cannot hold: `profile-ref`, `policy-ref`, `resize-blocked`, and `resize-blocked-at`.

  **Action required (the operator does not read the old annotations at all).** A workload still carrying only the old annotations after upgrade reads as unenrolled: it is dropped from measurement, its admission patches stop, and its `WorkloadProfile` eventually orphans. There is no dual-read window in the operator. Re-label every enrolled pod template: `measure` → `mode: measure`; `apply` (with `measure`) → `mode: apply`; `resize`/`autoresize` → `mode: resize`. Because enrollment is now a pod *label*, changing it re-rolls the workload like any other pod-template change. A soft rollover is possible at the workload level: set the `mode` label while leaving the old annotations in place, so a workload reads as enrolled to both the old and new operator version across the upgrade, then remove the annotations once the new version is confirmed.

### Fixed

- **Only opted-in pods are measured into a `WorkloadProfile`.** Measurement matched pods by the identity-tuple labels alone, so an unenrolled pod that happened to share those labels was folded into the profile. This was easy to hit with the default identity tuple (`app.kubernetes.io/name` + `app.kubernetes.io/component`), whose keys many pods carry. Collection now also requires the enrollment (`mode`) label, so a pod must both match the identity tuple and be opted in (to at least `measure`) to contribute samples.

## [0.3.18] - 2026-07-14

### Added

- **Restartable-init "native sidecar" containers are now right-sized.** Init containers with `restartPolicy: Always` (KEP-753 native sidecars) run for the pod's whole lifetime and are legitimate right-sizing targets, but were previously excluded from measurement along with run-to-completion init containers. Ballast now measures them, applies recommendations to `spec.initContainers` at admission, and resizes them in place — in-place resize of restartable-init containers rides the same `pods/resize` subresource as regular containers (supported on Kubernetes 1.33+, GA in 1.35). Run-to-completion init containers and ephemeral debug containers remain excluded; the axis is run-to-completion vs long-running, not init vs regular. This unblocks profiles that a long-lived injected sidecar (e.g. an OTel collector) could pin in `Accruing` indefinitely: the sidecar was measured just enough to be tracked but never accrued its own history, so the whole profile never reached `Sufficient` and its recommendations were never applied. (#30)

## [0.3.17] - 2026-07-06

### Fixed

- **The bundled Valkey sample store now persists to a PersistentVolumeClaim and is memory-bounded, so accrued history survives pod reschedules.** Previously the bundled Valkey ran with the subchart defaults: an `emptyDir` for `/data` and `resources: {}` (BestEffort QoS). That combination is the worst case for a sample store. BestEffort is the first thing the kubelet evicts under node memory pressure, and every eviction or reschedule wiped all samples and every first-seen marker. Because readiness gates on `minTimeSpan` (default 24h of observed history), a wipe silently reset the entire fleet to `Accruing` for 24h, and any further restart in that window restarted the clock, so on a busy cluster the fleet could stay perpetually `Accruing` without ever reaching `Sufficient`. The chart now enables `dataStorage` (a PVC on the cluster's default StorageClass; `valkey.dataStorage.className` pins a specific one) with `deploymentStrategy: Recreate` (a single-replica Deployment cannot roll onto a ReadWriteOnce PVC otherwise), sets `maxmemory 64mb` with `maxmemory-policy noeviction` (never evict first-seen markers; refuse writes loudly instead of being OOM-killed), and sets Valkey's memory request equal to its limit (`96Mi`) to lift it out of BestEffort. Valkey reloads its RDB snapshot from the PVC on start, so history is preserved across reschedules.

  The defaults are deliberately small (fine for dev and modest fleets). **They are linked and must be scaled together:** `maxmemory < memory limit <= memory request`, and PVC size `~= 2 × memory limit` (a background save writes a temporary RDB alongside the live one). The driver is dataset size, roughly `reservoirSize × (containers × metrics)`; raise `maxmemory`, the limit, the request, and the PVC in lockstep for larger clusters. See the annotated `valkey:` block in `values.yaml`.

  **Migration:** the first upgrade rolls the Valkey pod once (a final store reset, so the fleet re-accrues from that point) and provisions the PVC; there is no persistence across that single roll. Clusters using an external store (`valkey.enabled: false`) are unaffected.

## [0.3.16] - 2026-07-04

### Changed

- **The default policy's memory request is now sized at `p75` instead of `p50 * 1.05`.** In practice the `p50 * 1.05` request rode just below aggregate usage: each container's usage exceeds its own p50 half the time and memory working set is mildly right-skewed (mean > median), so the sum of `p50 * 1.05` across the fleet landed under the sum of current usage. For non-compressible memory that is the wrong side to sit on; the scheduler bin-packs against a claim that understates real occupancy. `p75` states occupancy directly (the request covers actual usage ~75% of the time), so the claim sits at or just above real usage, and it self-adjusts to each workload's spread instead of a fixed cushion on the median: a flat container's p75 is within a few percent of its p50 (no new waste, so this does not reintroduce the over-reservation the `avg * 1.25` formula caused), while a container with real variation gets proportionally more room. p75 stays clear of the tail (p95/p99) where brief startup spikes live, so it remains stable enough not to re-trip the 10% drift threshold; no headroom multiplier is applied. The `local-testing` preset's memory request entry changes the same way. Policies that state their own memory metric entries are unaffected; re-apply your policy manifests if you want existing custom policies to pick up the new shape.

## [0.3.15] - 2026-07-04

- **The ResourceAdjuster no longer re-evaluates every WorkloadProfile on each metrics poll.** Its watch previously had no predicate, so every status write the MetricsCollector makes (~once per poll, ~60s, as recommendation percentiles drift with fresh samples) woke the adjuster and made it re-list pods and recompute drift for that profile, roughly 15x more often than its resize interval implies. On large clusters that churn pinned operator CPU while doing no useful work (the adjuster reads whatever recommendations are current when it runs). The watch now filters updates to those the adjuster acts on: spec (`generation`) changes and `status.meetsThreshold` transitions still pass, so a profile that becomes ready is resized promptly, while the intermediate recommendation churn is ignored and pacing falls to the resize interval as intended. This also stops the `excluding drifted resources...` debug line from being emitted on every poll.

## [0.3.14] - 2026-07-04

### Changed

- **Demoted the "excluding drifted resources the resize subresource cannot mutate" log line from info to debug (`V(1)`).** This fires on every reconcile of a pod whose only drift is on a resource in-place resize cannot touch, which is expected steady-state behavior, not something an operator needs to see at info level. It was dominating operator logs. The message is unchanged and still emitted; raise log verbosity to `1` to see it.

## [0.3.13] - 2026-07-03

### Changed

- **Policy defaults now apply at resolve time instead of being baked into stored objects by the CRD schema.** All `+kubebuilder:default` markers are removed from the `ClusterResourcePolicy`/`ResourcePolicy` spec (`priority`, `metrics[].headroom`, and every `readiness` and `behaviors` field); the operator fills omitted fields with the canonical defaults (single source of truth: `api/v1/defaults.go`) when it resolves the policy for a workload. Rationale: admission-time defaults persist into the stored object at write time and are never revisited, so a policy written under an older release kept that release's defaults forever, even after a CRD upgrade changed them. In practice this pinned profiles at `Accruing`: a policy that omitted `cvMeanFloor` and predated 0.3.12 still carried the frozen 0.3.10 floors (`cpu: 10m`, `ephemeral-storage: 100Ki`) after upgrading, and no amount of re-applying the sparse manifest could fix it. With resolve-time defaulting, sparse policies always track the running release's defaults. The default values themselves are unchanged. Semantics worth noting: `kubectl get` now shows exactly what the author wrote (effective defaults are documented in the CRD field descriptions and README/DESIGN); an explicit empty `cvMeanFloor: {}` disables all floors so every resource gets the CV check; `minDataPoints: 0` is rejected by validation (omit the field for the default of 500, or set `1` for effectively no minimum); and an omitted `thresholds.resize.default` now genuinely falls through to `thresholds.default` per the documented coalesce order (previously, admission-time defaulting filled it to `20%` whenever the `resize` block was present, silently masking a custom `thresholds.default`).

  **Migration:** policies created under earlier releases still carry the old frozen defaults in their stored spec, which now read as explicit values. After upgrading, rewrite each policy from its sparse source manifest with `kubectl replace -f <file>` (`kubectl apply` cannot remove the frozen fields: they were never part of the applied configuration). Upgrade the chart before replacing policies, so the new operator is the one reading the sparse specs.

- **The default policy's memory request is now sized at `p50 * 1.05` instead of `avg * 1.25`.** The `avg * 1.25` formula (a mean / 0.80 utilization target) is right for CPU, which is spiky and idles far below its bursts, but memory working set is occupancy, not utilization: it is nearly flat per container, with p99 typically within ~15-30% of the mean. Applying the CPU formula to memory therefore put every request *above* the container's all-time observed p99 and pinned a fleet's aggregate memory reservation at 125% of aggregate usage; on a well-utilized cluster that walked total requests up toward allocatable as profiles became ready, shrinking schedulable headroom instead of reclaiming it. Spike protection is the limit's job (unchanged at `p99 * 1.2`); the request now states typical occupancy, using p50 for robustness to startup transients plus a 5% cushion so a typical pod sits just under its request without normal wobble re-tripping the drift threshold. The `local-testing` preset's memory request entry changes the same way. Policies that state their own memory metric entries are unaffected; re-apply your policy manifests if you want existing custom policies to pick up the new shape.

- **Lowered the default resize drift threshold from `20%` to `10%`.** A resize fires only when the current resource value has drifted from the recommendation by more than this threshold. In-place resize is a request/limit patch on a running pod with no restart, so it is cheap and safe to apply, and there is little reason to tolerate a wide staleness band: a recommendation that has moved more than 10% from the current value reflects a real shift in observed usage worth acting on, not noise. The canonical default lives in `api/v1/defaults.go` (`DefaultThreshold`), so sparse policies that omit `behaviors.thresholds` pick up `10%` automatically; the chart's built-in default policy states it explicitly as well. Policies that set an explicit threshold are unaffected.

## [0.3.12] - 2026-07-03

### Changed

- **Raised the default `readiness.cvMeanFloor` for CPU from `10m` to `25m` and for ephemeral storage from `100Ki` to `2Mi`.** The 0.3.10 floors proved too conservative in practice: workloads idling just above them (e.g. a few m of CPU or a few hundred Ki of scratch disk) still produce CVs dominated by quantization noise and startup spikes, pinning profiles at `Accruing`. Usage below the new floors remains far too small for a mis-sized recommendation to matter. The memory floor is unchanged at `25Mi`. As with 0.3.10, the defaults apply through CRD defaulting, so policies that don't set the field pick up the new floors after a CRD upgrade; policies that set `cvMeanFloor` explicitly are unaffected.

## [0.3.11] - 2026-07-03

### Fixed

- **OTLP metric export now uses delta temporality for counters and histograms, so single-increment series render on dashboards.** The OTel SDK exports a counter series only once it has data, so a series whose attribute set increments exactly once is born already at its final value — and a backend needs two samples of a *cumulative* series to compute an increase, so that lone increment never charts. This is not an edge case: `ballast.resize.applied` increments once per pod and then sits in cooldown for the whole resize interval, so the resize wave right after an operator restart (fresh series for every attribute set) was completely invisible — dashboards showed a huge burst of `cooldown` skips with zero applied resizes, which read as a broken panel. Delta exports carry each interval's increment directly, so a series' first export window charts correctly. Non-monotonic instruments (updown counters, gauges) remain cumulative, and the Prometheus `/metrics` endpoint is unaffected.

- **Failed in-place resizes now back off for one resize interval instead of retrying every reconcile.** The `resize-blocked` annotation was write-only: stamped `"true"` on failure but read by nothing, so a pod whose resize could not succeed was retried on every profile reconcile — each retry logging an error, emitting a `ResizeBlocked` warning event, and incrementing `ballast.resize.failed`, forever. The annotation now records the failure's error text (truncated to 256 characters) and a new `ballast.tightlinesoftware.com/resize-blocked-at` annotation records when it happened; evaluations within one resize interval of the failure are skipped with `ballast.resize.skipped{reason="blocked"}`, and a subsequent successful resize removes both annotations. Pods stamped `resize-blocked: "true"` by earlier versions carry no `resize-blocked-at` and are simply evaluated normally, so no migration is needed.

### Added

- **Resizes that would change a pod's QoS class are skipped as `qos_pinned` instead of failing forever.** Kubernetes fixes a pod's QoS class at creation and the resize subresource rejects any patch that would change it (`Pod QOS Class may not change as a result of resizing`). A `BestEffort` pod can therefore never gain requests in place, and a `Guaranteed` pod can only move requests and limits together. The resource adjuster now computes the QoS class before and after the planned adjustment and, when they differ, records `ballast.resize.skipped{reason="qos_pinned"}` without attempting the patch — previously such pods entered a permanent fail-retry loop of doomed patches, error logs, and warning events. `qos_pinned` pods still receive their recommendation at admission time (`apply`) when next recreated. Fuller QoS-class support (coupled request+limit moves for `Guaranteed` pods, a story for long-lived `BestEffort` pods) is tracked in [#48](https://github.com/Tight-Line/ballast/issues/48).

## [0.3.10] - 2026-07-02

### Added

- **New `readiness.cvMeanFloor` policy field unsticks near-idle workloads from `Accruing`.** The maxCV readiness check divides stddev by the mean, so a workload idling near zero produces a huge CV from measurement quantization and rare one-off spikes alone — a single ~750m startup burst among ~1700 samples of a ~1m-mean container yields CV ≈ 10 — and since profile readiness is an AND across all tracked resources, that one resource pinned the whole profile at `Accruing` forever, blocking recommendations for every other resource (including scaling down overstated reservations). `cvMeanFloor` maps a resource to a quantity below which its mean usage exempts it from the CV check; the sample-count and time-span gates still apply. Usage below the floor is too small for a mis-sized recommendation to matter: with the defaults (`cpu: 10m`, `memory: 25Mi`, `ephemeral-storage: 100Ki`) the resulting requests (~1.2 × mean) and limits (~1.2 × p99) stay far too small to hurt a node even when reached. Set a resource to `"0"` to always apply the CV check. The defaults apply through CRD defaulting, so after a CRD upgrade existing policies that don't set the field pick them up and previously stuck near-idle profiles flip to `Sufficient` on their next collection cycle.

## [0.3.9] - 2026-07-02

### Fixed

- **`maxChangePerCycle` now caps resize steps as a percentage of the gap to the recommendation, as documented.** The cap was computed against the current value instead of the current→recommended gap, so a badly underprovisioned workload converged backwards: tiny steps first, then exponentially larger ones (each cycle at most 1.5x the previous value with the default 50%), taking hours to correct a large gap. Steps are now capped at `maxChangePerCycle` percent of the remaining gap, so the first cycle makes the largest correction and later cycles refine it. To avoid the geometric tail never reaching the target (and parking a request just inside the drift threshold, which for the default policy could mean settling at the observed average usage with no headroom), a capped step that would land within the drift threshold of the recommendation applies the recommendation exactly; with the defaults (50% cap, 20% threshold) convergence completes in at most a few cycles from any starting point.

## [0.3.8] - 2026-07-02

### Fixed

- **Published chart packages ship their CRDs again, restoring first install on a bare cluster.** When `charts/ballast/crds/` became a gitignored build artifact (synced from `config/crd/bases/` by `make helm-build`), the release workflow's chart job kept packaging straight from a fresh checkout — which has no `crds/` — so every published package since then contained no CRDs. On a cluster without a prior install, `helm install` (or the install leg of `helm upgrade --install`) then failed manifest validation with `ensure CRDs are installed first`: the manifest includes the chart's own `BallastConfig`/`MetricsSource`/`ClusterResourcePolicy` objects, and Helm validates it before the pre-install CRD hook (0.3.3) ever runs, so the hook cannot rescue a first install. Existing clusters never noticed because the pre-upgrade hook keeps their CRDs in sync. The release workflow now syncs `config/crd/bases/*.yaml` into the chart before packaging, and `helm-build` creates the `crds/` directory first so the Make path also works from a fresh clone. Installs from older packages need the CRDs applied by hand (`kubectl apply -f config/crd/bases/`) before the first `helm install`.

## [0.3.7] - 2026-07-02

### Added

- **Project logos.** The full logo tops the README, and the Helm chart now declares an `icon` (the icon-shaped variant, served from the repo), which chart UIs such as Artifact Hub display next to the chart.

## [0.3.6] - 2026-07-02

### Fixed

- **Metrics from multiple replicas no longer merge into one series.** The chart now sets `service.instance.id` (the pod name, via the downward API) in `OTEL_RESOURCE_ATTRIBUTES`. Previously every replica exported identical resource attributes, so backends fingerprinted all replicas' samples into a single series. For cumulative counters emitted on every replica — the webhook-path metrics `ballast.webhook.mutations`, `ballast.apply.applied`, and `ballast.apply.skipped` — the interleaved per-process counts looked like a counter resetting on every sample, inflating `rate()`/`increase()` beyond any real activity; leader-only counters got the same distortion transiently at leader handoff. Gauges such as `ballast.profiles` appeared correct in steady state but double-counted during rollouts, when `service.version` briefly made the replicas distinguishable. With per-pod identity, each replica is its own series: counter math is correct (sum of per-instance increases), and gauge queries that aggregate across replicas should deduplicate explicitly (e.g. `max by (<identity tuple>)` before summing). Dashboards that relied on replicas being indistinguishable will see per-pod series after upgrading.

- **In-place resize no longer fails on pods with non-cpu/memory recommendations.** The Kubernetes pod resize subresource (KEP-1287) permits mutating only `cpu` and `memory`, but the resource adjuster built its patch from every drifted recommendation. A profile with a drifted `ephemeral-storage` recommendation (measured via the `kubelet-summary` source) therefore produced a patch the API server rejected with `Forbidden: only cpu and memory resources are mutable` — which failed the entire resize, including its legal cpu/memory changes, and marked the pod `resize-blocked`. Resize patches now include only cpu and memory; recommendations for other resources still apply at admission time through the webhook and take effect when the pod is recreated. Excluded drifted resources are logged, and when they are the only drift on a pod the skip is recorded as `ballast.resize.skipped{reason="not_resizable"}` (skip reasons describe the whole pod evaluation). Pods already annotated `ballast.tightlinesoftware.com/resize-blocked: "true"` by this failure must have the annotation removed (or be recreated) to resume resizing.

## [0.3.4] - 2026-07-02

### Added

- **New `ballast.apply.applied` and `ballast.apply.skipped` metrics.** `ballast.apply.applied` counts admission-time mutations that actually changed a pod's container resource requests or limits, carrying the same `profile`/`policy`/`namespace` attributes as `ballast.resize.applied` so dashboards can chart apply and resize activity side by side. Previously the only apply-side signal was `ballast.webhook.mutations{result="mutated"}`, which also counts patches that merely stamp annotations (such as the policy-ref) without touching resources, so "the webhook ran" and "resources were applied" were indistinguishable. `ballast.apply.skipped` counts admissions where the pod requested apply but nothing changed, with a `reason` attribute: `no_profile` (no WorkloadProfile exists yet for the workload's identity tuple), `not_ready` (profile exists but is below its history threshold), `no_change` (profile is ready but no container matched a recommendation), or `dry_run`. In particular, `no_profile` and `not_ready` (workloads that want recommendations but are not getting them yet) were previously invisible, folded into `ballast.webhook.mutations{result="skipped"}` together with pods that never requested apply. Exactly one of `apply.applied` / `apply.skipped` is recorded per admission that requests apply and finds its profile.

### Changed

- **`ballast.resize.skipped` now carries `policy` and `namespace` attributes**, matching `ballast.resize.applied`, `ballast.resize.failed`, and the new apply metrics. Both are empty for profile-level skips (`kill_switch`, `not_ready`, `no_policy`), which are not scoped to a single pod.

## [0.3.3] - 2026-07-02

### Fixed

- **CRD changes now apply on `helm upgrade`.** Helm installs the chart's `crds/` directory only on first install and never upgrades it, so every CRD change since a cluster's initial install was silently skipped (first visible symptom: 0.3.2's `STATE` printer column never appearing). A new pre-install/pre-upgrade hook Job runs `ballastd apply-crds`, a new operator subcommand that server-side-applies the CRD manifests baked into the image at build time (field manager `ballast-crd-installer`, force ownership). Applies are atomic and idempotent, so re-runs, concurrent runs, and manual invocations are all safe; running as `pre-install` also means the installer owns the CRD fields from day one, avoiding server-side-apply ownership conflicts with the initial Helm install. Disable with `crds.upgradeHook.enabled: false` if CRDs are managed by external tooling (upgrades then require applying `config/crd/bases/` by hand). The `crds/` directory remains for first-install bootstrapping, since Helm validates templated resources against cluster discovery before hooks run.

## [0.3.2] - 2026-07-02

### Added

- **Controller-runtime metrics now ship over OTLP.** The workqueue metrics (`workqueue_depth`, `workqueue_queue_duration_seconds`, `workqueue_work_duration_seconds`, `workqueue_adds_total`, `workqueue_retries_total`, and friends), reconcile metrics (`controller_runtime_reconcile_total`/`_errors_total`/`_time_seconds`, `controller_runtime_max_concurrent_reconciles`, `controller_runtime_active_workers`), plus the client-go and process/Go-runtime metrics from controller-runtime's Prometheus registry are bridged into the OTLP export stream via the OTel contrib Prometheus bridge, alongside the existing `ballast.*` instruments and with the same resource attributes. Previously these metrics were only available by scraping the optional Prometheus endpoint. Native `ballast.*` instruments are excluded from the bridge so enabling both telemetry paths does not export them twice under two spellings.

- **`kubectl get workloadprofiles` shows history sufficiency, and the `Ready` condition is now real (and means collection health).** A new `status.state` field (`Accruing` until the profile has sufficient history, then `Sufficient`) is maintained by the metrics collector alongside `meetsThreshold`, and the `STATE` printer column displays it. The API server defaults the field to `Accruing`, so profiles the collector has not yet visited (or that match no policy) show `Accruing` rather than an empty cell. Separately, the collector now maintains a `Ready` condition with conventional Kubernetes health semantics: `True` (reason `SamplesCollected`) when every resource the matched policy tracks produced at least one sample in the latest collection cycle, `False` (reason `MissingSamples`, naming the resources) when the measurement pipeline is not delivering. The two are deliberately orthogonal: a day-old profile that is measuring correctly is `Ready` while still `Accruing`, and a profile whose collection is silently broken (e.g. a wedged kubelet) shows `Ready: False` regardless of accumulated history, giving health dashboards a per-profile signal that did not exist before. Previously the `READY` printer column read a `Ready` condition that no component ever wrote, so it always rendered blank; that column is replaced by `STATE`.

### Fixed

- **`ballast.*` metrics were absent from the Prometheus `/metrics` endpoint.** With `telemetry.prometheus.enabled`, OTel instruments were registered into the client_golang default registry, but controller-runtime's metrics server serves its own registry, so the endpoint exposed only controller-runtime metrics and silently dropped every `ballast.*` series. Instruments now register into controller-runtime's registry and appear on `/metrics` as documented.

## [0.3.1] - 2026-07-02

### Fixed

- **Sustained full-CPU load on clusters with many pods.** Every `activeWorkloads` recount listed and deep-copied every pod in the cluster from the informer cache. The profile reconciler introduced in 0.3.0 performs such a recount on every `WorkloadProfile` event, including the metrics collector's once-per-poll status writes, so on a large cluster (thousands of pods, hundreds of profiles) the operator burned most of a core continuously, starting at startup and never settling. Pod lookups by profile now go through a cache field index keyed on the `profile-ref` annotation, so each recount touches only that profile's members. The pod reconciler's recounts and the profile-deletion fan-out use the same index. Counting semantics are unchanged, including the same-reconcile stamp override that keeps counts correct despite informer cache lag.

## [0.3.0] - 2026-07-01

This is a substantial change to how enrollment and profile convergence work; it
warrants a 0.3.0 minor release.

### Added

- **Deleting a `WorkloadProfile` now clears its Redis history.** A cleanup finalizer (`ballast.tightlinesoftware.com/profile-cleanup`) purges the profile's stored metric history before the object is removed. This runs on every deletion path, whether the operator's orphan-TTL sweep or a manual `kubectl delete workloadprofile`. Previously only the TTL sweep purged Redis, so a manual delete orphaned the history in Redis forever. On upgrade, the finalizer is back-filled onto existing profiles on their first reconcile, so no manual migration is required.

- **Prompt convergence watches.** The pod controller now watches `WorkloadProfile` deletions and `BallastConfig` `identityLabels` changes. A deleted profile that still has matching live pods is recreated within seconds (rather than waiting for a pod event or the informer resync period), and an `identityLabels` change promptly migrates affected pods.

### Changed

- **Enrollment is now fully level-triggered on live pod state rather than trusting the stamped `profile-ref`.** Each pod reconcile recomputes the pod's target profile from its current labels and the current `identityLabels`, then reconciles toward it:
  - **Identity change → migration.** Changing `identityLabels` (or a pod's own identity-label values) moves the pod to the newly-computed profile and recounts the profile it leaves so that profile can orphan and age out.
  - **Behavior-annotation removal → un-enrollment.** Stripping all Ballast behavior annotations from a running pod now removes its `profile-ref` and finalizer and decrements the profile's `activeWorkloads`. Previously the pod stayed enrolled and kept the profile from orphaning until the pod was deleted.
  - **Profile deletion with live pods → recreation.** Deleting a profile that still has matching workloads recreates it fresh (history reset) and, because the profile name is a deterministic function of identity, only matching workloads re-reference it.

  Behavior note: deleting a `WorkloadProfile` that still has matching live pods is now a **history reset, not a permanent removal** — the profile regenerates (empty) while matching pods exist. To permanently remove a profile, remove the workload's Ballast annotations (or delete its pods) first, then delete the profile.

### Fixed

- **Stale `activeWorkloads` counts now self-heal.** The profile reconciler independently recomputes each profile's count from live pod state on every profile event and informer resync, writing only on change. Previously a profile whose last referencing pod migrated away, un-enrolled, or disappeared without a processed delete event could keep a stale non-zero count forever, never orphan, and never age out (leaking the profile and its Redis history).

- **Profile identity labels in status now converge.** `status.tupleLabels` and `status.selectorLabels` are patched whenever they differ from the values recomputed on a member pod's reconcile, and the write is a conflict-free patch. Previously they were written exactly once at profile creation; if that write was lost (for example to a conflict with the cleanup-finalizer back-fill), the profile was never measured and its eventual Redis purge targeted the wrong key hash.

- **Kill-switch releases now converge within a minute.** Pod reconciles skipped while the kill switch is active requeue every minute instead of waiting for the informer resync, so enrollment work deferred during an outage (including an `identityLabels` fan-out) resumes promptly once the switch is released.

- **BallastConfig re-creation is a prompt convergence trigger.** The config watch now admits create events (filtered to the canonical BallastConfig name), so deleting and re-applying the config with different `identityLabels` migrates pods promptly instead of waiting for resync. A pod arriving while its profile is mid-deletion server-side (create returns AlreadyExists through a stale cache) now requeues instead of binding to the dying object.

## [0.2.5] - 2026-07-01

### Added

- **`ballast.profiles` gauge for profile-count and by-business-unit dashboards.** A new observable gauge emits a value of `1` per `WorkloadProfile`, carrying that profile's identity-tuple label values as attributes plus a `state` attribute (`accruing` until the profile meets its threshold, then `ready`). Totals aggregate by count; grouping by a tuple attribute (e.g. `business_unit`) breaks the fleet down by that dimension. The gauge reads the controller cache at collection time, so it is always a fresh snapshot with no counter drift.

- **Identity-tuple label values are now emitted as metric attributes.** Every profile-scoped metric (`samples.collected`, `fetch.errors`, `profiles.threshold_met`, `pods.processed`, `workload_profiles.created`/`purged`, `resize.applied`/`failed`/`skipped`, `webhook.mutations`) now carries one attribute per identity-tuple label. Each attribute key is the label's suffix (the segment after the last `/`) sanitized to `[a-z0-9_]` — e.g. `example.com/business-unit` becomes `business_unit` and `app.kubernetes.io/name` becomes `name`. If two labels would sanitize to the same suffix, the colliding ones fall back to their sanitized fully-qualified key so no attribute is dropped.

### Fixed

- **`ballast.samples.collected` was dead — it had no production caller and always read zero.** The counter is now incremented once per metric sample successfully written to the store (never on a write failure or in `--dry-run-measure`), carrying `source`, `resource`, `container`, and the profile's identity-tuple attributes.

### Changed

- **The `profile` attribute is now the readable profile name across all metrics.** `samples.collected` and `fetch.errors` previously carried the opaque tuple hash in place of a profile identifier; they now emit the same human-readable `profile` name as every other metric, alongside the identity-tuple attributes.

## [0.2.4] - 2026-07-01

### Added

- **OTel log export.** Ballast now ships its logs to an OTLP collector in addition to (or instead of) stdout. Log export reuses the existing `telemetry.otel` endpoint/protocol/insecure settings, so enabling OTel for metrics also ships logs by default; each structured log key is promoted to a top-level OTel log-record attribute. Set `logging.otel.enabled: false` to keep metrics on OTLP but not logs. New knobs under `logging`:
  - `logging.stdout.enabled` (default `true`) writes logs to stderr; set `false` to ship only via OTLP.
  - `logging.stdout.additionalKeys` (default `{}`) adds static key/values to stdout JSON lines only, never to the OTLP path. For example `additionalKeys: {otlp: true}` lets a stdout log collector skip lines already exported over OTLP and avoid double-ingesting them. This is intentionally not a Ballast default.
  - `logging.otel.enabled` (default `true`) ships logs to the configured OTLP collector when `telemetry.otel.enabled` is true.

  Corresponding operator flags: `--log-stdout`, `--log-stdout-fields` (a JSON object), and `--log-otel-enabled`.

### Fixed

- **Per-component log-level overrides had no effect.** The `logging.webhook/watcher/collector/adjuster` values (and their `--log-level-*` flags) were parsed but never applied, so every component logged at the global `logging.level`. The logger now gates each entry by a per-component level keyed on the logger name, and each controller's reconcile logger is named accordingly, so e.g. `logging.collector: debug` lowers only the metrics collector's verbosity. (Naming the controller loggers drops the `controllerGroup`/`controllerKind` fields controller-runtime added by default; `namespace`/`name` are retained.)

## [0.2.3] - 2026-07-01

### Changed

- **Default policy limits now carry 20% headroom instead of sizing at the observed peak.** The `homogeneous-large-fleet` default `ClusterResourcePolicy` set its memory and ephemeral-storage limits at `p99` with no headroom, which OOMKilled (or evicted) any pod that exceeded its observed p99 — including legitimate rare spikes whose true peak was never in the sample window. Both limits are now `p99 * 1.2`: the 20% margin absorbs a normal spike while still catching a genuine leak or runaway. Requests (`avg * 1.25`) and the intentionally-omitted CPU limit are unchanged. The `local-testing` preset's memory limit follows suit.

### Fixed

- **`kubeletSummary` could permanently lose a healthy node's metrics after a transient kubelet blip.** Each node's `/stats/summary` is cached per-node (`CacheTTL` 55s); once an entry aged past `2*CacheTTL`, the staleness check returned before attempting any refresh, so `fetchTime` never advanced and the node was skipped forever, emitting `skipping node: summary too stale` on every scrape with an ever-growing `age` even after the kubelet recovered. A refresh is now attempted whenever the cache is past `CacheTTL`; the staleness gate only governs whether cached data is served as a fallback when that refresh fails. A node that has recovered heals within one `CacheTTL` of the kubelet coming back.

- **`kubeletSummary` had no rate limit on retries against a failing node.** Fetch failures on a previously-cached node return stale data rather than a hard error, so the per-workload backoff never engaged for them. Combined with the recovery fix above, a down kubelet would have been re-probed once per enrolled workload every scrape. Each node now carries a `lastAttempt` timestamp that gates refreshes to at most one probe per `CacheTTL` per node, regardless of how many workload identities scrape in a cycle.

## [0.2.2] - 2026-07-01

### Fixed

- **Init and ephemeral containers were being measured.** The metrics API and kubelet summary return a flat container list that does not mark init or ephemeral containers, so the collector recorded samples for them and produced `WorkloadProfile` container entries that could never be applied (apply and resize only patch `spec.containers`). The collector now excludes all init containers and ephemeral debug containers from measurement, reading their names from the profile's pod specs. This is intentionally broad for now: restartable-init "native sidecar" containers are long-running and would be good right-sizing targets, but supporting them requires extending the apply and resize lanes to `spec.initContainers` — tracked in [#30](https://github.com/Tight-Line/ballast/issues/30). Enrollment stays annotation-driven with no Job/CronJob carve-out; docs now steer users away from annotating Job pod specs and toward caution with CronJob pod specs.

## [0.2.1] - 2026-07-01

### Fixed

- **`kubeletSummary` metrics collection failed with a `nodes is forbidden` RBAC error.** The plugin lists nodes before proxying to each kubelet's `/stats/summary` endpoint, but the ballast `ClusterRole` only granted `nodes/proxy` `get`, not `nodes` `list`. Collection of `ephemeral-storage` samples failed on every cycle with `nodes is forbidden ... cannot list resource "nodes"`; CPU and memory (served via `metrics.k8s.io`) were unaffected. The `ClusterRole` now grants `get` and `list` on `nodes`.

## [0.2.0] - 2026-06-30

### Added

- **`p75` and `p90` aggregations.** `ClusterResourcePolicy` metric entries now accept `p75` and `p90` alongside the existing `p50`/`p95`/`p99`/`max`/`avg`. The stats engine computes both percentiles, the metrics collector publishes them on `WorkloadProfile` container usage stats, and the recommendation resolver maps them through. This unblocks sizing ephemeral storage at p90.

- **Memory-limit and ephemeral-storage sizing in the default policy.** The default `ClusterResourcePolicy` now sets a memory limit at p99 with no headroom (a pod exceeding its production-observed peak is likely leaking and should be OOMKilled), which yields Burstable QoS. It also sizes ephemeral-storage request and limit at p90 and p99 from the `kubelet-summary` source. CPU limits remain intentionally omitted to avoid throttling.

- **Per-metric `source` in the policy template.** Each metric entry can now name its own `MetricsSource`, falling back to the policy default when absent. This lets CPU/memory entries reference `kubernetes-metrics` and ephemeral-storage entries reference `kubelet-summary` within one policy object.

- **Policy presets.** The chart ships a catalog of policy presets as Helm values overlays under `charts/ballast/presets/`. The built-in default in `values.yaml` is the `homogeneous-large-fleet` preset; `local-testing.yaml` is a fast-cycle overlay for kind clusters that `make helm-update-local` applies automatically. Select one at install time with `-f`; a later `-f` or `--set` deep-merges on top. See `charts/ballast/presets/README.md`.

- **`TESTING.md`.** A local test loop walkthrough: deploy with the local-testing preset, install a workload with the autoresize annotation, then watch the profile become eligible and the pod get resized.

### Changed

- **Default request sizing moved from `p95 * headroom` to `avg * 1.25`** (= mean / 0.80 target utilization). For a large homogeneous fleet the aggregate pressure is predictable, so sizing requests at 80% of mean keeps nodes dense while leaving headroom for normal variation.

- **Default sampling and readiness retuned for production cadence.** The `defaultMetricsSources` poll interval rose from 60s to 5m, and `defaultClusterResourcePolicy.readiness.minDataPoints` dropped from 500 to 250. A single long-running pod accrues ~288 samples over 24h at the 5m cadence, so the 24h `minTimeSpan` rather than the sample count becomes the binding constraint for a lone pod to become eligible.

## [0.1.8] - 2026-06-30

### Added

- **`kubeletSummary` plugin (ephemeral storage metrics).** A new plugin type reads the kubelet Summary API via the Kubernetes API server proxy (`GET /api/v1/nodes/{name}/proxy/stats/summary`) and reports `ephemeral-storage` usage per pod. The API is accessed cluster-wide with no extra credentials — the ballast `ClusterRole` gains a `nodes/proxy` `get` rule. Per-node summaries are cached (default 55 s TTL); entries older than 2×TTL are skipped with a warning rather than returned as stale data. Because the Summary API reports storage at the pod level rather than per container, usage is distributed evenly across a pod's containers (documented limitation). Per-workload exponential backoff applies on node fetch errors.

  The Helm chart gains a `defaultMetricsSources.kubeletSummary` entry (enabled by default) that installs a `MetricsSource` named `kubelet-summary`.

### Changed

- **`defaultMetricsSources` map replaces `defaultMetricsSource`.** The `values.yaml` key `defaultMetricsSource` (singular) is replaced by `defaultMetricsSources` (plural), a map keyed by plugin type (`kubernetesMetrics`, `kubeletSummary`). This groups all default sources under one key and makes it easy to add future sources without top-level sprawl. Override individual entries with `--set defaultMetricsSources.kubeletSummary.enabled=false` etc.

## [0.1.7] - 2026-06-30

### Fixed

- **`activeWorkloads` drifts to zero after a rollout restart.** The old `incrementActiveWorkloads`/`decrementActiveWorkloads` pair used read-modify-write against the informer cache. The cache is eventually consistent: with `replicas=2`, four patches fire in rapid succession during a restart, and at least one stale read overwrites a valid increment, driving `activeWorkloads` to 0 and triggering a false `Orphaned` condition. Replaced both functions with `setActiveWorkloads`, which lists all pods carrying the Ballast finalizer with a matching `profile-ref` annotation and a zero `DeletionTimestamp`, then writes that count directly to the WorkloadProfile status. Every reconcile is now idempotent and self-healing; drift is impossible regardless of reconcile ordering or cache lag.

- **`policy-ref` annotation is ambiguous when policies share a name across scopes.** A `ClusterResourcePolicy` and a `ResourcePolicy` can share the same name, and `ResourcePolicy` objects in different namespaces can also share a name. The webhook previously stored only the bare policy name in the `ballast.tightlinesoftware.com/policy-ref` annotation, making it impossible to distinguish these cases. Namespace-scoped `ResourcePolicy` refs are now stored as `namespace/name`; `ClusterResourcePolicy` refs keep the bare name.

- **Workload watcher could overwrite webhook-applied resource requests via a stale `Update`.** When adding or removing the Ballast finalizer, the pod reconciler used `client.Update` (a full object write) instead of `client.Patch`. With in-place pod resize enabled, `spec.containers[*].resources` is no longer an immutable field; a full Update carrying a cache-stale pod spec could silently overwrite the admission webhook's resource recommendations. All three finalizer mutation callsites now use `client.Patch` with a `MergeFrom` base, so only the finalizer diff is sent and the pod spec is never touched.

## [0.1.6] - 2026-06-29

### Added

- **Prometheus and OpenTelemetry metrics.** All five Ballast components (MetricsCollector, WorkloadWatcher, ResourceAdjuster, Admission Webhook, KillSwitch) now publish operational counters via OpenTelemetry. A `MeterProvider` can expose metrics on the existing `/metrics` endpoint as a Prometheus scrape target, push to an OTLP collector, or both. Prometheus is enabled automatically when `--metrics-bind-address` is not `"0"`; OTLP push requires `--otel-metrics-endpoint`.

  New flags: `--otel-metrics-endpoint`, `--otel-metrics-protocol` (`grpc`/`http`), `--otel-metrics-interval`, `--otel-metrics-insecure`.

  The Helm chart gains a `telemetry:` section for toggling Prometheus (with an optional `ServiceMonitor` for Prometheus Operator) and OTLP independently. OTel service resource attributes (`service.name`, `service.version`, `service.namespace`) are injected via `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` environment variables derived from `telemetry.serviceName`, `telemetry.serviceNamespace`, and `Chart.AppVersion`; defaults can be overridden at runtime without a chart change.

### Fixed

- `main()` refactored into `run() int` so the deferred `shutdownMetrics` call runs correctly before process exit (`os.Exit` is now only called from `main`, which carries no defers).

## [0.1.5] - 2026-06-29

### Fixed

- **In-place resize now respects the configured interval.** The resource adjuster reconciles whenever the WorkloadProfile status is updated (every ~1 minute from the metrics collector), not just on its own timer. Without a cooldown check, every status update triggered a fresh resize even though the policy interval was set to 15 minutes. The adjuster now checks `ballast.tightlinesoftware.com/last-resize` on each pod and skips it if a resize was applied within the configured interval.

### Changed

- Demoted two high-frequency log messages from `Info` to `Debug` (`V(1)`): "resolved policy" (emitted by the policy resolver on every reconciliation) and "profile does not meet threshold, skipping resize" (emitted by the resource adjuster). Both are now suppressed unless the controller is started with `--zap-log-level=debug`.

## [0.1.4] - 2026-06-29

### Added

- Helm chart now installs a default `MetricsSource` (`kubernetes-metrics`) wired to the built-in Kubernetes Metrics API and a default `ClusterResourcePolicy` (`default`) with conservative CPU/memory request sizing on first install. Both can be disabled with `defaultMetricsSource.enabled: false` / `defaultClusterResourcePolicy.enabled: false` in values.
- Local kind cluster development workflow: `make docker-kind KIND_CLUSTER=<name>` (build image for host arch + load into kind), `make helm-install-local` (install chart with local image), `make helm-update-local KIND_CLUSTER=<name>` (combined one-shot rebuild and redeploy). Host CPU architecture is detected automatically via `uname -m`.

### Fixed

- **Critical:** The `kubernetesMetrics` plugin was never registered in `main.go`, so the metrics collector silently skipped every poll cycle on every install. Metrics collection was completely non-functional out of the box.
- **Critical:** The metrics collector used `WorkloadProfile.status.tupleLabels` (which contains human-readable placeholder values like `nocomponent` for absent identity labels) as the Kubernetes label selector when querying the metrics API. This meant pods were never found and metrics were never collected for any workload missing an identity label. A new `status.selectorLabels` field now stores the selector separately: real label values are used as-is, and absent keys carry a `"--missing--"` sentinel. The `kubernetesMetrics` plugin now filters pod metrics client-side against these requirements, because the `metrics.k8s.io` API ignores label selectors server-side and returns all pods regardless.
- The `kubernetesMetrics` plugin now caches the full pod metrics list for `CacheTTL` (default 55s) and serves all concurrent `FetchStats` calls from that cache. Previously every WorkloadProfile reconcile triggered a separate metrics API call; with hundreds of profiles sharing a poll cycle this would have hammered the metrics-server once per profile per minute.
- `ClusterRole` was missing the `update` verb on `pods` (required for adding/removing the workloadwatcher finalizer), had no rule for the `pods/resize` subresource (required for in-place resize), and had no rule for `metrics.k8s.io/pods` (required for the metrics collector to call the Kubernetes Metrics API). All three omissions caused `403 Forbidden` errors at runtime.
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
