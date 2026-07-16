# Local testing

This walks through exercising the full measure → apply → resize loop on a local
[kind](https://kind.sigs.k8s.io/) cluster, using the `local-testing` policy preset
so you see results in minutes instead of the 24h the production default requires.

## Prerequisites

- A running kind cluster
- [metrics-server](https://github.com/kubernetes-sigs/metrics-server) installed in
  it (Ballast measures CPU and memory through the Metrics API; ephemeral storage
  comes from the kubelet Summary API, which needs no extra component). On kind you
  typically need `--set args={--kubelet-insecure-tls}`.
- [cert-manager](https://cert-manager.io/) installed (the admission webhook needs a
  CA bundle)
- Kubernetes 1.35+ for in-place resize; on older versions measure and apply work
  but resize does not.

## 1. Deploy Ballast with the local-testing preset

```sh
make helm-update-local KIND_CLUSTER=<name>
```

This builds the image for your host arch, loads it into the kind cluster, and
installs the chart with `-f charts/ballast/presets/local-testing.yaml`. That preset
polls every 15s and lets the default policy act after 5 samples over 1 minute with a
30s resize interval. (See `charts/ballast/presets/README.md` for the catalog.)

## 2. Install a workload that opts in

```sh
helm install nginx bitnami/nginx \
  --namespace nginx --create-namespace \
  --set commonLabels."ballast\.tightlinesoftware\.com/mode"=resize
```

The `mode: resize` label opts the workload into measure + apply + resize. The
rest of this guide assumes the `nginx` namespace from `--namespace nginx` above.

## 3. Watch the profile become eligible

```sh
kubectl get -w workloadprofile nginx--nocomponent
```

`meetsThreshold` flips to `true` after ~5 samples (~1 minute at the 15s poll
interval).

## 4. Watch admission stamp resources on new pods

```sh
kubectl get -w pods -n nginx
```

The next pod created after the profile is ready gets resources applied at admission.
If the out-of-the-box requests (nginx ships ~50m CPU and generous memory) differ
from the profile recommendation by more than the 20% drift threshold, the
ResourceAdjuster resizes the running pod at the 30s interval.

## 5. Confirm the resize

```sh
kubectl describe pod <name> -n nginx
```

Check that the `ballast.tightlinesoftware.com/last-resize` annotation is stamped and
that the container resources match the profile recommendation.

## Cleanup

```sh
helm uninstall nginx
helm uninstall ballast -n ballast-system
```
