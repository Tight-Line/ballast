# Policy presets

Ballast ships a catalog of policy presets: values overlays that retune the default
`ClusterResourcePolicy` (and the supporting `MetricsSource` poll intervals) for a
particular operating profile. Pick one at install time with `-f`.

Each preset is a normal Helm values file, so it composes with base `values.yaml`
and any later `-f`/`--set` you pass: map fields deep-merge, list fields (like
`defaultClusterResourcePolicy.metrics`) are replaced wholesale, and the
right-most source wins. That means you can layer a preset and then override a
single field, e.g.:

```sh
helm upgrade --install ballast ./charts/ballast \
  -f charts/ballast/presets/local-testing.yaml \
  --set defaultClusterResourcePolicy.behaviors.resize.interval=10s
```

## Available presets

| Preset | File | Use case |
| --- | --- | --- |
| `homogeneous-large-fleet` | _(built-in default in `values.yaml`)_ | Production fleets of many similar pods. Sizes CPU requests at `avg * 1.25`, memory requests at `p50 * 1.05`, memory limit at `p99 * 1.2`, and ephemeral storage from the kubelet Summary API. Polls every 5m and requires 250 samples over 24h before acting. |
| `local-testing` | `local-testing.yaml` | Local kind clusters. Polls every 15s and acts after 5 samples over 1m with a 30s resize interval, so you can watch a workload become eligible and get resized within minutes. **Never use in production.** |

The `homogeneous-large-fleet` preset is the chart's built-in default, so a plain
`helm install` with no `-f` already gives you a production-sane policy. To add a
new preset, drop a values overlay in this directory and document it in the table
above.
