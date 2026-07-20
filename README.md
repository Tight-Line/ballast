<p align="center">
  <img src="docs/images/ballast-logo-full.png" alt="Ballast — three ballast tanks trimming a ship to level" width="440">
</p>

# Ballast

[![Known Vulnerabilities](https://snyk.io/test/github/Tight-Line/ballast/badge.svg)](https://snyk.io/test/github/Tight-Line/ballast)
[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=Tight-Line_ballast&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=Tight-Line_ballast)
[![codecov](https://codecov.io/gh/Tight-Line/ballast/branch/main/graph/badge.svg)](https://codecov.io/gh/Tight-Line/ballast)

Ballast is a Kubernetes operator that automatically right-sizes workload resource requests and limits based on real operational history. It is a more active alternative to [Fairwinds Goldilocks](https://github.com/FairwindsOps/goldilocks): rather than suggesting changes, it applies them — at admission time and on running pods via in-place resize (Kubernetes 1.35+).

## What it does

<p align="center">
  <img src="docs/images/cpu-requests-vs-usage.png" alt="A cluster's CPU requests pinned against allocatable capacity, then dropping to track real usage once Ballast takes over" width="820">
</p>

This is one cluster's CPU over three days. The purple line is allocatable capacity; the blue line is the sum of pod requests — what the scheduler believes is claimed; the red line is what those pods actually burn.

For the first two days requests ride right up against the ceiling. On paper the cluster is full and can't schedule another pod, yet real usage runs at less than half the reservation. That gap is pure waste: capacity paid for, reserved, and never used.

Partway through 7/3 the workloads are enrolled in Ballast. Requests drop to track observed usage, freeing roughly half the cluster's schedulable CPU without touching a single running workload. That reclaimed headroom is the point: the same nodes now fit far more work, or you run the same work on fewer nodes.

## Why not just use VPA?

The [Vertical Pod Autoscaler](https://kubernetes.io/docs/concepts/workloads/autoscaling/vertical-pod-autoscale/) is the obvious tool for this job, and for a single stable, long-lived workload it works well. Ballast exists because VPA ties a workload's usage history to that one workload's identity in one namespace, and three everyday situations throw that history away and drop you back to guessing:

- **Cold start.** VPA learns from nothing. Its recommender aggregates usage into decaying histograms (memory in 24-hour peak windows over an 8-day default history), so a freshly deployed workload has no samples and gets a low-confidence, deliberately padded recommendation until days of real traffic accrue.
- **No cross-namespace memory.** VPA keys its history on the tuple `(namespace, container name, pod labels)`, and that key is not adjustable — it is derived automatically, and a VPA object is itself namespaced, so it physically cannot aggregate pods from another namespace. Deploy the same app (`service: billing`, `component: backend-api`, `profile: dev`) into a second namespace, say a different developer's own test namespace, and the namespace in that key changes, so VPA sees a brand-new workload and starts from zero. Every namespace relearns the same app independently. Cold start, again.
- **Redeploy gap.** A workload's history lives in its VPA object's checkpoints (`VerticalPodAutoscalerCheckpoint` resources owned by the VPA object). Delete the workload and its VPA object, as any GitOps teardown or `helm uninstall` does, and Kubernetes garbage-collects the checkpoints along with it. Redeploy a minute later and there is no history left to recover. Cold start, a third time.

Ballast fixes all three by decoupling history from the workload object. It keys usage on a **workload identity tuple** you choose (a set of pod labels; see [WorkloadProfile Identity](#workloadprofile-identity)) and stores it cluster-wide in Redis/Valkey, independent of namespace and independent of any single workload's lifecycle. Forty dev namespaces running the same app all feed one well-sampled profile, so a brand-new deployment — in a fresh namespace, or right after a teardown — inherits the fleet's accumulated history and is sized correctly from its very first admission instead of relearning from scratch.

## How it works

Workloads opt in with a single label on their pod templates. Ballast observes real CPU, memory, and ephemeral-storage utilization, accumulates a rolling history keyed to a *workload identity tuple* (a set of pod labels you configure), and uses that history to right-size all three resources across an escalating ladder of behaviors:

1. **measure** — collect per-container usage samples into a time-series store (Redis/Valkey).
2. **apply** — also patch resource requests and limits at admission time when a pod is created (implies measure).
3. **resize** — also adjust resources on running pods via the Kubernetes in-place resize API (1.35+) (implies apply).

You choose a rung with the `ballast.tightlinesoftware.com/mode` label; each rung includes everything below it. Nothing is applied or resized until the `WorkloadProfile` meets its readiness threshold — that is normal behavior for any resize-enabled workload.

Pod eviction for cluster rebalancing is handled by [Kubernetes Descheduler](https://github.com/kubernetes-sigs/descheduler) — see the Enrollment section for details.

## Prerequisites

- Kubernetes 1.35+ (required for in-place pod resize; earlier versions support measure and apply but not resize)
- [metrics-server](https://github.com/kubernetes-sigs/metrics-server) installed in the cluster (source for CPU and memory; ephemeral-storage usage comes from the kubelet Summary API and needs no extra component)
- TLS certificate for the admission webhook (see [Webhook TLS](#webhook-tls) below)
- A Redis-compatible store (Ballast ships with a bundled Valkey via Helm; an existing Redis or Valkey instance works too). The bundled Valkey persists to a PersistentVolumeClaim on the cluster's default StorageClass by default, so accrued history survives pod reschedules; its memory and storage defaults are small and linked, so scale them together (see the annotated `valkey:` block in `values.yaml`).

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

Upgrades are `helm upgrade --install` with no extra steps. CRDs are kept in sync automatically: Helm itself never upgrades the `crds/` directory, so the chart runs a pre-install/pre-upgrade hook Job (`ballastd apply-crds`) that server-side-applies the CRD manifests baked into the operator image. Set `crds.upgradeHook.enabled: false` to opt out if external tooling manages CRDs; you are then responsible for applying `config/crd/bases/` on every upgrade.

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

If your cluster runs dev, staging, and production side-by-side and you want separate profiles per environment, add `ballast.tightlinesoftware.com/resource-profile` to the identity tuple and apply it to your pods:

```yaml
# BallastConfig / Helm values
ballastConfig:
  identityLabels:
    - app.kubernetes.io/name
    - app.kubernetes.io/component
    - ballast.tightlinesoftware.com/resource-profile
```

```yaml
# Pod template labels
labels:
  app.kubernetes.io/name: billing
  app.kubernetes.io/component: api
  ballast.tightlinesoftware.com/resource-profile: prod
```

Now `(billing, api, prod)` and `(billing, api, dev)` are measured independently. Pods without the `ballast.tightlinesoftware.com/resource-profile` label get a placeholder value in the profile name (`noresourceprofile`) rather than being skipped, so opted-in pods always produce a profile.

> **Changing `identityLabels` wipes your operational history.** It redefines what constitutes a workload identity, so all existing `WorkloadProfile` objects are renamed and their accumulated Redis history is orphaned. Ballast starts fresh from zero samples. Plan your tuple before enrolling workloads.

## Enrollment

Enroll a workload by setting the `ballast.tightlinesoftware.com/mode` label on its pod template. Ballast never acts on a workload without explicit opt-in. The value is a single rung on an escalating ladder; each rung includes everything below it.

| `ballast.tightlinesoftware.com/mode` | Behavior |
|---|---|
| `measure` | Collect metrics only |
| `apply` | measure, plus patch requests/limits at admission time |
| `resize` | apply, plus adjust resources on running pods via in-place resize |

Enrollment is a **label**, not an annotation, so the API server can filter on it server-side. Ballast's controllers watch and its admission webhook fires only for pods that carry the label, which keeps the operator's footprint proportional to the number of enrolled pods rather than the total pod count; this matters on very large clusters ([#55](https://github.com/Tight-Line/ballast/issues/55)). A pod that carries the label with any other value is rejected by the webhook.

Ballast's own outputs stay annotations, since they carry values a label cannot hold (timestamps, slash-separated refs): `profile-ref`, `policy-ref`, `resize-blocked`, and `resize-blocked-at`.

**`apply` and `resize` do not cover the same resources.** At admission time the webhook can set *any* recommended resource — cpu, memory, ephemeral-storage — because it patches the pod spec before the pod exists. In-place resize is narrower by Kubernetes design: the pod resize subresource ([KEP-1287](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)) permits mutating only `cpu` and `memory` on a running pod, and rejects a patch that touches anything else. Ballast therefore excludes all other resources from resize patches. In practice this means an `ephemeral-storage` recommendation takes effect only when a pod is recreated (via `apply`), never in place; a running pod whose ephemeral-storage drifts will keep its current value until its next restart. Ballast logs every exclusion; when non-resizable drift is the *only* drift on a pod (so no resize is issued at all), it also records `ballast.resize.skipped{reason="not_resizable"}` — skip reasons always describe the whole pod, never a single resource axis.

**`resize` cannot change a pod's QoS class.** A pod's QoS class (`BestEffort`, `Burstable`, `Guaranteed`) is fixed at creation, and the resize subresource rejects any patch that would change it. Two recommendation shapes run into this: a `BestEffort` pod (no requests or limits on any container) can never gain requests in place, and a `Guaranteed` pod (requests equal to limits for cpu and memory everywhere) can only be resized by moving requests and limits together. Ballast detects both before patching and records `ballast.resize.skipped{reason="qos_pinned"}` instead of attempting a resize that cannot succeed; the recommendation still applies at admission time (via `apply`) when the pod is next recreated. When a resize fails for a reason Ballast could not predict, the pod is annotated `ballast.tightlinesoftware.com/resize-blocked` with the error text plus `resize-blocked-at` with the failure time, and further attempts are skipped (`reason="blocked"`) until one resize interval has elapsed; a later successful resize clears both annotations.

**Pod eviction** is deliberately out of scope for Ballast. Ballast keeps resource requests and limits accurate; cluster rebalancing based on those corrected values is best handled by [Kubernetes Descheduler](https://github.com/kubernetes-sigs/descheduler) (specifically its `LowNodeUtilization` strategy). This is a clean division of labor: Ballast gets the weight right, Descheduler decides where pods should sit.

**Example — full automation:**

```yaml
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: billing
        ballast.tightlinesoftware.com/resource-profile: prod
        ballast.tightlinesoftware.com/mode: resize
```

**Example — measure only (safe first step):**

```yaml
spec:
  template:
    metadata:
      labels:
        ballast.tightlinesoftware.com/mode: measure
```

### Which workloads to enroll

Ballast right-sizes **long-running** workloads by learning their steady-state usage over hours or days (default readiness: 250 samples over 24 hours). Enroll Deployments, StatefulSets, DaemonSets, and similar controllers whose pods run continuously.

- **Job pods: you almost certainly do not want to enroll them.** A Job runs to completion, often in seconds or minutes, so it never accumulates enough steady-state history to cross the readiness threshold. Enrolling a Job's pod template only creates a `WorkloadProfile` that never produces a recommendation. Ballast will not stop you (opt-in is entirely under your control), but there is nothing to gain and it clutters your profiles.
- **CronJob pods: think hard before enrolling them.** A CronJob creates Jobs, and those Jobs create the pods (`CronJob → Job → Pod`), so CronJob pods carry the same run-to-completion caveat as any other Job pod. Enroll them only if each run is genuinely long-lived and resource-stable enough to measure meaningfully — for example, a multi-hour nightly batch job. A short or spiky periodic task is a poor fit and will mostly generate noise.

**Regular containers and restartable-init sidecars are right-sized.** On an enrolled pod, Ballast measures and resizes the pod's regular `spec.containers` **and** its restartable-init "native sidecar" containers (`restartPolicy: Always`, KEP-753) — these run for the pod's whole lifetime and are patched on `spec.initContainers` just like regular containers. It excludes **run-to-completion init containers and ephemeral debug containers** from measurement — there is no per-container knob to configure; the exclusion is automatic. The distinction is run-to-completion vs long-running, not init vs regular. In-place resize of restartable-init containers rides the same `pods/resize` subresource as regular containers (supported on Kubernetes 1.33+, GA in 1.35).

### Bulk enrollment with `scripts/enroll.sh`

Setting the `mode` label by hand across an existing cluster is tedious. `scripts/enroll.sh` does it in bulk: it labels every Deployment, StatefulSet, and DaemonSet whose pod template already carries the full identity tuple and is not yet enrolled. The tuple is read from the `BallastConfig` named `ballast` in the target context (falling back to `--identity-labels`, then the chart default), so the script enrolls exactly the workloads the operator will key on.

It is dry-run by default; pass `--apply` to make changes. `--mode` (one of `measure`, `apply`, `resize`) is required.

```bash
# See what would be enrolled at measure, cluster-wide (no changes)
scripts/enroll.sh --mode measure

# Enroll the 'web' namespace at measure
scripts/enroll.sh --mode measure -n web --apply
```

How each workload is handled depends on whether a restart is safe:

- **More than one replica:** the label is added to the template and the workload is rolling-restarted (`kubectl rollout restart` semantics), so its pods pick the label up while staying available. The script waits on `kubectl rollout status` (see `--timeout`).
- **A single replica, or an `OnDelete` update strategy:** restarting would mean downtime, so the label is added to the template durably *without* a restart, and the live pods are labeled in place. The `mode` label is not part of any selector, so this is a pure metadata edit; the operator picks the pods up and enrolls them the same as if they had been recreated.

No workloads are skipped by default; the no-restart route makes it safe to sweep everything. Pass `--ignore` a workload-name regex to carve some out (e.g. `--ignore 'consul|vault'`). Run with `--help` for the full option list.

Enrollment is idempotent, so it is safe to re-run: a workload that already carries the `mode` label is never re-patched or restarted. Those already at the requested mode are reported as a no-op count; one already at a *different* mode is flagged with a warning and left unchanged (unless `--remode` is set; see below).

> On the no-restart path a pod is enrolled and counted immediately; *when* it gets sized then depends on the mode. `apply` patches resources only at admission, so a pod labeled in place keeps its current resources until it is next recreated. `resize` also adjusts running pods in place, so a pod labeled in place is resized at runtime once its profile is ready — no restart needed. (`measure` never sizes.)

#### Fast first-enroll, fast mode-upgrade

Two flags make rolling out enrollment across a large cluster quick, in two stages:

- **`--no-restart` (hotfix in place):** enroll *without* recreating any pods, even multi-replica ones. The label is added to each workload's template durably (via the same pause/adopt, partition, and OnDelete techniques used for single-replica workloads) and the live pods are labeled in place. This is much faster than rolling every workload, at the cost of one caveat: at `--mode apply`, in-place pods keep their current resources until they next restart (`measure` collects either way, and `resize` adjusts running pods in place regardless). A great first pass:

  ```bash
  scripts/enroll.sh --mode measure --no-restart --apply    # enroll the whole cluster at measure, no restarts
  ```

- **`--remode`:** by default a workload already enrolled at a *different* mode is left alone (warned). `--remode` changes it to `--mode` instead, so you can promote the fleet up the ladder once profiles are ready. Combine with `--no-restart` to do it in place:

  ```bash
  scripts/enroll.sh --mode resize --remode --no-restart --apply   # upgrade measure/apply workloads to resize, in place
  ```

  A workload already at the requested mode stays a no-op. Because `resize` adjusts running pods in place, upgrading to `resize` with `--no-restart` starts right-sizing the live pods without a single restart.

## Verifying a WorkloadProfile

Once a pod carrying the `ballast.tightlinesoftware.com/mode` label is running, Ballast creates a `WorkloadProfile` for its identity tuple. Check it with:

```bash
kubectl get workloadprofiles
kubectl describe workloadprofile billing--api--prod
```

The profile status shows accumulated usage statistics and recommendations once the readiness threshold is met (default: 250 samples collected over 24 hours). CPU, memory, and ephemeral storage are all tracked and sized:

```yaml
status:
  containers:
    - name: app
      usageStats:
        - resource: cpu
          samples: 288
          mean: "230m"
          p95: "240m"
          p99: "310m"
          cv: "0.46"
        - resource: memory
          samples: 288
          mean: "180Mi"
          p50: "176Mi"
          p75: "192Mi"
          p95: "210Mi"
          p99: "240Mi"
          cv: "0.21"
        - resource: ephemeral-storage
          samples: 288
          p90: "1200Mi"
          p99: "1800Mi"
          cv: "0.33"
      recommendations:
        cpu:
          request: "288m"     # avg * 1.25 headroom
        memory:
          request: "192Mi"    # p75
          limit: "288Mi"      # p99 * 1.2
        ephemeral-storage:
          request: "1200Mi"   # p90
          limit: "2160Mi"     # p99 * 1.2
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

A fresh `helm install` ships three objects out of the box so measurements work without any extra setup: two `MetricsSource` objects (CPU/memory and ephemeral storage) and one catch-all `ClusterResourcePolicy`.

### MetricsSource: `kubernetes-metrics`

```yaml
spec:
  type: kubernetesMetrics
  config:
    pollInterval: "300s"
    reservoirSize: 10000
```

This wires Ballast to the cluster's [metrics-server](https://github.com/kubernetes-sigs/metrics-server) (which must already be installed — it is not bundled) for CPU and memory. Samples are collected every 5 minutes and up to 10,000 samples per container per metric are retained in Redis.

### MetricsSource: `kubelet-summary`

```yaml
spec:
  type: kubeletSummary
  config:
    pollInterval: "300s"
    reservoirSize: 10000
```

This reads ephemeral-storage usage from the kubelet Summary API (via the API server proxy). No extra credentials are needed beyond the Ballast ServiceAccount.

To opt out of either source and manage `MetricsSource` objects yourself, set `enabled: false` on the relevant entry:

```yaml
# values.yaml
defaultMetricsSources:
  kubernetesMetrics:
    enabled: false
  kubeletSummary:
    enabled: false
```

### ClusterResourcePolicy: `default`

This is the `homogeneous-large-fleet` preset, the chart's built-in default:

```yaml
spec:
  priority: 0
  metrics:
    - resource: cpu
      field: request
      source: kubernetes-metrics
      aggregation: avg
      headroom: "1.25"
    - resource: memory
      field: request
      source: kubernetes-metrics
      aggregation: p50
      headroom: "1.05"
    - resource: memory
      field: limit
      source: kubernetes-metrics
      aggregation: p99
      headroom: "1.2"
    - resource: ephemeral-storage
      field: request
      source: kubelet-summary
      aggregation: p90
      headroom: "1.0"
    - resource: ephemeral-storage
      field: limit
      source: kubelet-summary
      aggregation: p99
      headroom: "1.2"
  readiness:
    minDataPoints: 250
    minTimeSpan: "24h"
    maxCV: "1.5"
    cvMeanFloor:
      cpu: "25m"
      memory: "25Mi"
      ephemeral-storage: "2Mi"
  behaviors:
    thresholds:
      default: "10%"
    resize:
      maxChangePerCycle: "50%"
      interval: "15m"
```

This catch-all policy applies to every opted-in pod in the cluster. Key design decisions:

- **CPU request at `avg * 1.25`.** CPU usage is spiky and idles far below its bursts, so sizing the request at 80% of mean (= mean / 0.80 target utilization) keeps nodes dense while leaving headroom for normal variation. For a large homogeneous fleet the aggregate pressure is predictable, so the mean is a reliable basis.
- **Memory request at `p75`, deliberately not the CPU formula.** Memory working set is occupancy, not utilization: it is nearly flat per container (p99 typically sits within ~15-30% of the mean), so a utilization-target formula like `avg * 1.25` would put every request above the container's all-time observed p99 and pin the fleet's aggregate reservation at 125% of aggregate usage, all of it reserved-but-unusable capacity. Spike protection is the limit's job, not the request's; the request only needs to state typical occupancy so the scheduler's claim matches reality. p75 states that occupancy directly: the request covers actual usage ~75% of the time, so the scheduler's claim sits at or just above real usage rather than under it, and it self-adjusts to each workload's spread (a flat container's p75 is within a few percent of its p50, while a variable one gets proportionally more room). p75 stays clear of the tail where brief startup spikes live, so it remains stable enough not to re-trip the drift threshold.
- **Memory limit at `p99 * 1.2`.** p99 is the highest usage the workload has shown in production; the 20% headroom absorbs a normal rare spike while still OOMKilling a pod that runs well past its observed peak (a likely leak). This yields Burstable QoS (limit > request), the right class for most production workloads. **CPU limits are intentionally omitted** — they cause throttling rather than reclaiming waste.
- **Ephemeral storage from the kubelet Summary API.** The request is sized at p90 (the growth-skewed distribution) and the limit at `p99 * 1.2` so the kubelet evicts a genuine runaway pod before the node hits disk pressure while tolerating a normal spike above the observed peak.
- **250 samples over 24 hours before acting.** At the 5-minute poll interval a single long-running pod accrues ~288 samples in 24h, so the 24h window — not the sample count — is the binding constraint. A high coefficient of variation (CV > 1.5) also blocks action — it means the workload is too spiky to size reliably. The CV check is skipped when mean usage sits below a tiny per-resource floor (`cvMeanFloor`, defaults: 25m CPU, 25Mi memory, 2Mi ephemeral-storage): CV divides by the mean, so near-idle workloads produce huge CVs from quantization noise and rare startup spikes alone, and without the floor a single near-idle resource would pin the whole profile at `Accruing` forever — blocking recommendations for every other resource. Usage below the floor is too small for a mis-sized recommendation to matter.
- **10% drift threshold.** A resize only fires when the current resource value deviates from the recommendation by more than 10%. In-place resize is cheap and safe (a request/limit patch on a running pod, no restart), so the band is deliberately tight: a recommendation that has moved more than 10% reflects a real shift in observed usage worth acting on, not noise.
- **50% max change per cycle.** Each resize moves at most half the remaining gap between the current value and the recommendation, giving workloads time to stabilize between adjustments. The first step makes most of the correction; once a step would land within the drift threshold, the recommendation is applied exactly, so convergence completes instead of stalling just inside the threshold.
- **Priority 0.** This is the lowest possible priority. Any `ClusterResourcePolicy` or `ResourcePolicy` with `priority > 0` wins for matched workloads, so you can override specific namespaces or workload kinds without touching this default.

### Policy presets

The default above is one entry in a catalog of presets — Helm values overlays under [`charts/ballast/presets/`](charts/ballast/presets/README.md) that retune the policy for a particular operating profile. `homogeneous-large-fleet` is built into `values.yaml`; `local-testing` is a fast-cycle overlay for kind clusters. Select one at install time with `-f`:

```bash
helm install ballast ballast/ballast -n ballast-system --create-namespace \
  -f charts/ballast/presets/local-testing.yaml
```

A later `-f` file or `--set` deep-merges on top (map fields merge; list fields like `metrics` are replaced wholesale), so you can layer a preset and override a single field.

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
      aggregation: avg
      headroom: "1.2"
    - resource: memory
      field: request
      aggregation: avg
      headroom: "1.2"
```

> **Note:** `metrics` is a list, so overriding it replaces the entire default list (including the memory-limit and ephemeral-storage entries). Repeat every entry you want to keep.

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

State only your intentional deviations: any `readiness` or `behaviors` field you omit (here, `cvMeanFloor` and all of `behaviors`) is filled with the documented default by the operator **when the policy is resolved**, so sparse policies automatically track the current release's defaults across upgrades. Nothing is baked into the stored object at write time. `kubectl get` shows only what you wrote; `kubectl explain clusterresourcepolicy.spec.readiness` documents the effective defaults. To disable the `cvMeanFloor` exemption entirely, set it to an explicit empty map (`cvMeanFloor: {}`).

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

# Install metrics-server (required for the kubernetesMetrics plugin)
# kind nodes don't have valid kubelet certs, so --kubelet-insecure-tls is required
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl patch deployment metrics-server -n kube-system \
  --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
kubectl rollout status deployment/metrics-server -n kube-system
```

**Iterate: change code → rebuild → redeploy**

```bash
make helm-update-local KIND_CLUSTER=ballast-dev
```

This runs three steps in sequence:

1. **Build** — `docker build --platform linux/<host-arch>` tagged `:local`. The host architecture is detected automatically via `uname -m`, so the same command works on both ARM and x86 machines.
2. **Load** — `kind load docker-image` injects the image directly into the kind node; no registry push or GHCR credentials needed.
3. **Install** — `helm upgrade --install` deploys the chart into `ballast-system` with `image.pullPolicy=Never`, pinning it to the locally loaded image, and applies the `local-testing` policy preset (`-f charts/ballast/presets/local-testing.yaml`) for fast feedback.

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

For the full measure → apply → resize walkthrough on a local cluster, see [TESTING.md](TESTING.md).

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
