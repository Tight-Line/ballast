#!/bin/bash
#
# Render-time checks for the Helm chart's valkey storage settings.
#
# Usage:
#   ./scripts/test-chart.sh [chart-dir]     # default: charts/ballast
#
# Runs `helm template` with a handful of value sets and asserts on the output
# or the failure message. The chart's subchart must already be present under
# <chart-dir>/charts/ (`make helm-build` does that; `make helm-test` runs it
# first).
#
# These run without a cluster, so `lookup` returns empty: the cluster-aware
# guards in templates/valkey-storage-guard.yaml (no default StorageClass,
# requestedSize below a live PVC's capacity) render nothing here by design and
# are not exercised. What is checked is that they stay silent without a cluster.

set -euo pipefail

CHART_DIR="${1:-charts/ballast}"
HELM="${HELM:-helm}"
failures=0

render() {
    "$HELM" template ballast "$CHART_DIR" --namespace ballast-system "$@" 2>&1
}

# pvc prints the valkey PersistentVolumeClaim document from a render.
pvc() {
    awk '
        /^---/ { if (found) printf "%s", doc; doc = ""; found = 0; next }
        { doc = doc $0 "\n" }
        /^kind: PersistentVolumeClaim/ { found = 1 }
        END { if (found) printf "%s", doc }
    ' <<<"$1"
}

pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; echo "$2" | sed 's/^/       /' | head -20; failures=$((failures + 1)); }

# 1. Defaults: renders, PVC requests 1Gi, no storageClassName.
name="defaults render a 1Gi PVC with no storageClassName"
if out=$(render); then
    doc=$(pvc "$out")
    if grep -q 'storage: 1Gi' <<<"$doc" && ! grep -q 'storageClassName' <<<"$doc"; then
        pass "$name"
    else
        fail "$name" "$doc"
    fi
else
    fail "$name" "$out"
fi

# 2. Named class renders storageClassName.
name="className renders storageClassName"
if out=$(render --set valkey.dataStorage.className=fast-ssd); then
    doc=$(pvc "$out")
    if grep -q 'storageClassName: fast-ssd' <<<"$doc"; then
        pass "$name"
    else
        fail "$name" "$doc"
    fi
else
    fail "$name" "$out"
fi

# 3. Named class wins even with the cluster-default acknowledgement off.
name="className with useClusterDefaultClass=false renders storageClassName"
if out=$(render --set valkey.dataStorage.className=fast-ssd --set valkey.dataStorage.useClusterDefaultClass=false); then
    doc=$(pvc "$out")
    if grep -q 'storageClassName: fast-ssd' <<<"$doc"; then
        pass "$name"
    else
        fail "$name" "$doc"
    fi
else
    fail "$name" "$out"
fi

# 4. No class and no cluster-default acknowledgement fails with the message.
name="empty className without useClusterDefaultClass fails"
if out=$(render --set valkey.dataStorage.className= --set valkey.dataStorage.useClusterDefaultClass=false); then
    fail "$name" "rendered successfully; expected a failure"
elif grep -q 'valkey.dataStorage needs a StorageClass decision' <<<"$out"; then
    pass "$name"
else
    fail "$name" "$out"
fi

# 5. The decision is only required when the subchart renders a PVC.
name="no StorageClass decision needed when dataStorage is disabled"
if out=$(render --set valkey.dataStorage.useClusterDefaultClass=false --set valkey.dataStorage.enabled=false); then
    if ! grep -q 'kind: PersistentVolumeClaim' <<<"$out"; then
        pass "$name"
    else
        fail "$name" "$(pvc "$out")"
    fi
else
    fail "$name" "$out"
fi

# 6. The guard template itself never emits an object.
name="valkey-storage-guard.yaml renders no objects"
if out=$(render --show-only templates/valkey-storage-guard.yaml); then
    fail "$name" "$out"
elif grep -q 'could not find template' <<<"$out"; then
    pass "$name"
else
    fail "$name" "$out"
fi

if [[ $failures -gt 0 ]]; then
    echo "$failures chart check(s) failed"
    exit 1
fi
echo "All chart checks passed"
