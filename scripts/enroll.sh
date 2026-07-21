#!/usr/bin/env bash
#
# enroll-by-identity-labels.sh
#
# Opts Deployments / StatefulSets / DaemonSets (and their pods) INTO Ballast by
# adding the enrollment label
#
#   ballast.tightlinesoftware.com/mode: <measure|apply|resize>
#
# to every workload whose pod template already carries the full identity tuple
# and is not yet enrolled. A workload qualifies when its
# .spec.template.metadata.labels has ALL of the identity-tuple keys set to a
# non-empty value AND does NOT already carry ballast.tightlinesoftware.com/mode.
#
# The identity tuple is resolved in this order:
#   1. --identity-labels, if given (explicit override).
#   2. the live BallastConfig named "ballast" in the target context
#      (.spec.identityLabels) — the same tuple the operator itself keys on, so
#      the script's notion of "qualifying" stays in lockstep with the operator.
#   3. the chart default (app.kubernetes.io/name + app.kubernetes.io/component)
#      when no BallastConfig is reachable.
#
# This is a bulk opt-IN: it enrolls workloads that are not yet managed by Ballast.
# It is deliberately willing to restart multi-replica workloads so their pods
# actually pick the label up, and only avoids a restart where one would hurt.
#
# Restart policy (the point of the script):
#
#   > 1 replica    Patch the template to add the label together with a
#                  kubectl.kubernetes.io/restartedAt stamp, then wait on
#                  `kubectl rollout status`. One clean rolling update enrolls the
#                  pods; a multi-replica rollout keeps the workload available.
#                  ("replicas" for a DaemonSet means status.desiredNumberScheduled.)
#   <= 1 replica   A single-pod (or scaled-to-zero) workload cannot tolerate a
#     or OnDelete   rolling restart, and an OnDelete workload opts out of auto-
#                  restart by design. These take a no-restart route: the label is
#                  added to the template durably (pause/adopt for Deployments, the
#                  partition dance for StatefulSets, the OnDelete swap for
#                  DaemonSets), and the live pods are labeled in place. The mode
#                  label is not part of any selector, so a live pod patch is a
#                  pure metadata edit and never triggers a restart.
#
# No workloads are excluded by default. Because the no-restart route (see below)
# can enroll even stateful infrastructure in place without recreating pods, a
# bulk sweep is safe to run against everything. Use --ignore to skip workloads by
# name when you do want to carve some out (e.g. --ignore 'consul|vault').
#
# Safety properties:
#   * Dry-run by default. Nothing is written without --apply.
#   * Owners that are paused or mid-rollout are skipped with a reason; re-run
#     after resolving.
#   * On the no-restart route the pod UID set is verified unchanged afterwards,
#     and for Deployments that no new ReplicaSet appeared.
#   * Idempotent: an already-enrolled template no longer qualifies, so re-running
#     converges. Safe to re-run until the summary is clean.
#
# Note: if these workloads are managed by GitOps/Helm, also add the label to the
# source manifests, or the template change will be reverted on the next sync.

set -euo pipefail

DOMAIN="ballast.tightlinesoftware.com"
MODE_LABEL="${DOMAIN}/mode"
RESTART_ANN="kubectl.kubernetes.io/restartedAt"

# The identity tuple every qualifying workload must already carry. Defaults to
# Ballast's chart default (charts/ballast/values.yaml: ballastConfig.identityLabels);
# override with --identity-labels to match a cluster's configured tuple.
DEFAULT_IDENTITY_LABELS="app.kubernetes.io/name,app.kubernetes.io/component"
IDENTITY_LABELS=()

DO_APPLY=false
MODE=""
NAMESPACE=""
CONTEXT=""
IDENTITY_CSV=""
IGNORE_REGEX=""
FORCE_NO_RESTART=false
REMODE=false
WAIT_TIMEOUT=300
SETTLE=8

usage() {
  cat <<EOF
Usage: $(basename "$0") --mode measure|apply|resize [--apply] [-n NAMESPACE]
                        [--context CTX] [--identity-labels a,b,c] [--ignore REGEX]
                        [--timeout SECS] [--settle SECS]

Bulk-enrolls Deployments/StatefulSets/DaemonSets (and their pods) into Ballast by
adding ${MODE_LABEL}=<mode> to every workload whose pod template already carries
every identity-tuple label and is not already enrolled. The tuple is read from
the BallastConfig 'ballast' in the target context, or --identity-labels, or the
chart default (${DEFAULT_IDENTITY_LABELS}).

By default multi-replica workloads are rolling-restarted so their pods pick up
the label, while single-replica (or OnDelete) workloads are enrolled without any
restart. --no-restart forces the no-restart route for every workload (faster, no
recreate). --remode also re-labels workloads already enrolled at a different mode
instead of leaving them alone.

Options:
  --mode             Enrollment rung to apply: measure, apply, or resize (required).
  --apply            Perform the enrollment. Without this flag the script only
                     prints the plan (dry-run).
  -n, --namespace    Restrict to one namespace (default: all namespaces).
  --context          kubectl context to use (default: current context).
  --identity-labels  Comma-separated pod-label keys a workload must carry to
                     qualify. Overrides the tuple read from BallastConfig
                     'ballast'. Chart-default fallback: ${DEFAULT_IDENTITY_LABELS}
  --ignore           Regex of workload names to skip (default: none; enrolls
                     everything). E.g. --ignore 'consul|vault' to carve out
                     stateful infrastructure.
  --no-restart       Hotfix in place: add/change the label on every workload's
                     template durably and label its live pods, with no rolling
                     restart even for multi-replica workloads. Faster (no pod
                     recreate). Note: at --mode apply, in-place pods keep their
                     current resources until they next restart; measure and
                     resize are unaffected.
  --remode           Also act on workloads already enrolled at a *different* mode,
                     changing them to --mode (default: leave them alone with a
                     warning). Workloads already at --mode stay a no-op.
  --timeout          Seconds to wait for a multi-replica rollout to complete
                     (default: ${WAIT_TIMEOUT}). A timeout is a warning, not a failure.
  --settle           Seconds to wait before post-enrollment verification on the
                     no-restart route (default: ${SETTLE}).
  -h, --help         Show this help.

Examples:
  $(basename "$0") --mode measure                    # dry-run, all namespaces
  $(basename "$0") --mode measure --apply            # enroll everything at measure
  $(basename "$0") --mode apply -n web --apply       # enroll namespace 'web' at apply
  $(basename "$0") --mode resize --ignore 'consul|vault' --apply # skip stateful infra
  $(basename "$0") --mode measure --no-restart --apply       # fast first enroll, no restarts
  $(basename "$0") --mode resize --remode --no-restart --apply # fast mode upgrade in place
  $(basename "$0") --mode measure --identity-labels app.kubernetes.io/name,app.kubernetes.io/component,tier
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode) MODE="$2"; shift 2 ;;
    --apply) DO_APPLY=true; shift ;;
    -n|--namespace) NAMESPACE="$2"; shift 2 ;;
    --context) CONTEXT="$2"; shift 2 ;;
    --identity-labels) IDENTITY_CSV="$2"; shift 2 ;;
    --ignore) IGNORE_REGEX="$2"; shift 2 ;;
    --no-restart) FORCE_NO_RESTART=true; shift ;;
    --remode) REMODE=true; shift ;;
    --timeout) WAIT_TIMEOUT="$2"; shift 2 ;;
    --settle) SETTLE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 1 ;;
  esac
done

case "$MODE" in
  measure|apply|resize) ;;
  "") echo "--mode is required (measure|apply|resize)" >&2; usage >&2; exit 1 ;;
  *) echo "invalid --mode '$MODE' (want measure|apply|resize)" >&2; exit 1 ;;
esac

command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
command -v kubectl >/dev/null || { echo "kubectl is required" >&2; exit 1; }

NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)

k() {
  if [[ -n "$CONTEXT" ]]; then
    kubectl --context "$CONTEXT" "$@"
  else
    kubectl "$@"
  fi
}

NS_ARGS=(--all-namespaces)
if [[ -n "$NAMESPACE" ]]; then
  NS_ARGS=(-n "$NAMESPACE")
fi

info() { echo "$*"; }
warn() { echo "WARNING: $*" >&2; }

# resolve_identity_labels: fills IDENTITY_LABELS / IDENTITY_SOURCE / IDENTITY_JSON.
# Precedence: explicit --identity-labels, then the live BallastConfig's
# .spec.identityLabels (the tuple the operator itself keys on), then the chart
# default. The BallastConfig read honors --context via k().
resolve_identity_labels() {
  local csv src cfg
  if [[ -n "$IDENTITY_CSV" ]]; then
    csv="$IDENTITY_CSV"; src="--identity-labels"
  elif cfg=$(k get ballastconfig ballast -o json 2>/dev/null) \
       && csv=$(jq -r '(.spec.identityLabels // []) | join(",")' <<<"$cfg") \
       && [[ -n "$csv" ]]; then
    src="BallastConfig 'ballast' (context: ${CONTEXT:-current})"
  else
    csv="$DEFAULT_IDENTITY_LABELS"; src="chart default (no BallastConfig 'ballast' reachable)"
  fi

  local raw lbl
  IFS=',' read -r -a raw <<<"$csv"
  IDENTITY_LABELS=()
  for lbl in "${raw[@]}"; do
    lbl="${lbl#"${lbl%%[![:space:]]*}"}"  # ltrim
    lbl="${lbl%"${lbl##*[![:space:]]}"}"  # rtrim
    [[ -n "$lbl" ]] && IDENTITY_LABELS+=("$lbl")
  done
  if [[ ${#IDENTITY_LABELS[@]} -eq 0 ]]; then
    echo "no identity labels resolved (source: ${src})" >&2
    exit 1
  fi
  IDENTITY_SOURCE="$src"
  IDENTITY_JSON=$(printf '%s\n' "${IDENTITY_LABELS[@]}" | jq -R . | jq -sc .)
}
resolve_identity_labels

TMP_DIR=$(mktemp -d)
RESULTS_FILE="$TMP_DIR/results"
: >"$RESULTS_FILE"
# Tally of deployments found already fully enrolled (template, ReplicaSet, and all
# pods at $MODE); reported as a single summary line rather than one row each.
CONVERGED_FILE="$TMP_DIR/converged"
: >"$CONVERGED_FILE"

# INFLIGHT holds a recovery breadcrumb while an owner is mid no-restart edit, so
# an interrupt (Ctrl-C, network drop) tells the user exactly how to recover.
INFLIGHT=""
on_exit() {
  if [[ -n "$INFLIGHT" ]]; then
    echo "" >&2
    echo "INTERRUPTED MID-ENROLLMENT: $INFLIGHT" >&2
  fi
  rm -rf "$TMP_DIR"
}
trap on_exit EXIT

# record STATUS "kind ns/name" "detail". STATUS: PLAN OK WARN SKIP FAIL NOTE.
# Prints immediately (no-restart owner edits take ~15-20s each; a silent apply run
# is indistinguishable from a hung one) and appends for the summary counts.
record() {
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$RESULTS_FILE"
  printf '%-5s %-55s %s\n' "$1" "$2" "$3"
}

# jq prelude. hastuple: every configured identity label present and non-empty.
# modeval: the current mode-label value ("" when absent). qualifies (the default
# enrollment target): has the tuple AND is not yet enrolled. qualifies_remode
# (the --remode target): has the tuple AND is not already at the requested mode
# (so it covers unenrolled pods and pods enrolled at a different mode).
QUALIFY_DEFS='
  def ne($o; k): ((($o[k]) // "") | tostring) != "";
  def hastuple($lbls): ($idlabels | map(ne($lbls; .)) | all);
  def modeval($lbls): ((($lbls[$mlbl]) // "") | tostring);
  def qualifies($lbls): hastuple($lbls) and (ne($lbls; $mlbl) | not);
  def qualifies_remode($lbls): hastuple($lbls) and (modeval($lbls) != $mode);
'
JQ_QUAL_ARGS=(--argjson idlabels "$IDENTITY_JSON" --arg mlbl "$MODE_LABEL" --arg mode "$MODE")

# The active qualifier: --remode widens the target to include different-mode workloads.
if $REMODE; then QUAL_FN="qualifies_remode"; else QUAL_FN="qualifies"; fi

# owner_qualifies <id> <obj-json>: recheck the fetched template against the active
# qualifier (apply mode reads live, so it may have changed since the scan). Records
# SKIP and returns 1 when the workload no longer qualifies.
owner_qualifies() {
  local id=$1 obj=$2 ok
  ok=$(jq -r "${JQ_QUAL_ARGS[@]}" \
    "$QUALIFY_DEFS"' if '"$QUAL_FN"'(.spec.template.metadata.labels // {}) then "yes" else "no" end' \
    <<<"$obj")
  if [[ "$ok" != "yes" ]]; then
    record SKIP "$id" "template no longer qualifies (already at ${MODE}, or missing an identity label)"
    return 1
  fi
  return 0
}

# mode_action <obj-json>: human phrase for the label change on this workload,
# reading the current mode from the template. "set …=X" for a fresh enroll,
# "re-mode … Y->X" when --remode is changing an existing enrollment.
mode_action() {
  local prev
  prev=$(jq -r --arg l "$MODE_LABEL" '.spec.template.metadata.labels[$l] // ""' <<<"$1")
  if [[ -z "$prev" ]]; then
    printf 'set %s=%s' "$MODE_LABEL" "$MODE"
  else
    printf 're-mode %s %s->%s' "$MODE_LABEL" "$prev" "$MODE"
  fi
}

# build_template_patch <mode> <with_restart:true|false>: merge patch that adds the
# mode label to the pod template, plus a restartedAt stamp when restarting.
build_template_patch() {
  local mode=$1 restart=$2
  if [[ "$restart" == "true" ]]; then
    jq -cn --arg l "$MODE_LABEL" --arg v "$mode" --arg ra "$RESTART_ANN" --arg now "$NOW" \
      '{spec:{template:{metadata:{labels:{($l):$v},annotations:{($ra):$now}}}}}'
  else
    jq -cn --arg l "$MODE_LABEL" --arg v "$mode" \
      '{spec:{template:{metadata:{labels:{($l):$v}}}}}'
  fi
}

# selector_of <json>: matchLabels as "k=v,k=v", or "" when matchExpressions are in
# play (pod-set verification / in-place pod labeling is then skipped, not guessed).
selector_of() {
  jq -r '
    if ((.spec.selector.matchExpressions // []) | length) > 0 then ""
    else ((.spec.selector.matchLabels // {}) | to_entries | map("\(.key)=\(.value)") | join(","))
    end' <<<"$1"
}

# get_obj <kind> <ns> <name> <cache-file>: one object as JSON. Apply mode re-reads
# the live object (paused/settled checks must be fresh right before mutating);
# dry-run reads the scan-phase cache.
get_obj() {
  if $DO_APPLY; then
    k get "$1" -n "$2" "$3" -o json
  else
    jq --arg ns "$2" --arg n "$3" \
      '[.items[] | select(.metadata.namespace == $ns and .metadata.name == $n)][0] // {}' "$4"
  fi
}

# rs_list_for <ns>: the namespace's ReplicaSets, live in apply mode so a deploy that
# lands mid-run is seen (the current ReplicaSet is matched against the live
# Deployment's revision), from the scan cache in dry-run.
rs_list_for() {
  if $DO_APPLY; then
    k get replicasets -n "$1" -o json
  else
    jq --arg ns "$1" '{items: [.items[] | select(.metadata.namespace == $ns)]}' \
      "$TMP_DIR/replicasets.json"
  fi
}

# snapshot_uids <ns> <selector>: sorted UIDs of non-terminating pods.
snapshot_uids() {
  k get pods -n "$1" -l "$2" -o json \
    | jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .metadata.uid] | sort | join(",")'
}

# verify_owner <id> <ns> <selector> <before-uids> <success-detail>: confirms the
# in-place route did not restart anything, by diffing the set of RUNNING (non-
# terminating) pod UIDs against the snapshot taken before the edit. Caller sleeps
# $SETTLE first. This diff is the reliable signal: a real rollout removes a running
# pod from the set (and adds a new one), whereas a pod that was ALREADY terminating
# before the edit — e.g. a finalizer-wedged ghost — was never in `before` and so
# cannot trip a false alarm. (The old check counted any terminating pod as a
# failure, which misfired precisely on the ghosts this tool exists to clean up.)
verify_owner() {
  local id=$1 ns=$2 sel=$3 before=$4 okmsg=$5
  if [[ -z "$sel" ]]; then
    record OK "$id" "$okmsg (pod verification skipped: selector uses matchExpressions)"
    return 0
  fi
  local after
  after=$(k get pods -n "$ns" -l "$sel" -o json \
    | jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .metadata.uid] | sort | join(",")')
  if [[ "$after" == "$before" ]]; then
    record OK "$id" "$okmsg (running-pod set unchanged)"
  else
    record WARN "$id" "$okmsg, but the running-pod set changed during the window (a rollout may have started, or unrelated churn/HPA); verify manually"
  fi
}

wait_until() { # <timeout-secs> <fn> [args...]
  local timeout=$1; shift
  local start=$SECONDS
  while ! "$@"; do
    if (( SECONDS - start >= timeout )); then
      return 1
    fi
    sleep 2
  done
}

# patch_pods_mode <ns> <selector> <mode> <id>: add the mode label to EVERY pod the
# selector matches, in any phase — running, pending, succeeded (Completed), failed
# (Error), and even while terminating. Pure metadata (the label is in no selector),
# so no pod restarts. Two reasons to cover every state: (1) it is the actual
# enrollment on the no-restart route, the template patch only makes it durable for
# future pods; (2) a pod that lacks the mode label is invisible to the operator's
# label-scoped informer cache, so a terminating one stays wedged on its finalizer
# forever — labeling it is what lets the operator run the finalizer and release it.
# Pods already at the mode are skipped, so re-runs stay idempotent and cheap.
patch_pods_mode() {
  local ns=$1 sel=$2 mode=$3 id=$4
  if [[ -z "$sel" ]]; then
    record WARN "$id" "selector uses matchExpressions; label pods manually with ${MODE_LABEL}=${mode}"
    return 0
  fi
  local names pod failed=0 patch
  patch=$(jq -cn --arg l "$MODE_LABEL" --arg v "$mode" '{metadata:{labels:{($l):$v}}}')
  names=$(k get pods -n "$ns" -l "$sel" -o json \
    | jq -r --arg l "$MODE_LABEL" --arg v "$mode" \
        '.items[] | select((.metadata.labels[$l] // "") != $v) | .metadata.name')
  [[ -z "$names" ]] && return 0
  while IFS= read -r pod; do
    [[ -z "$pod" ]] && continue
    k patch pod -n "$ns" "$pod" --type merge -p "$patch" >/dev/null 2>&1 || failed=$((failed + 1))
  done <<<"$names"
  if (( failed > 0 )); then
    warn "$id: failed to label $failed pod(s) with ${MODE_LABEL}=${mode} (some may have already terminated); re-run to converge"
  fi
}

# pods_missing_mode <ns> <selector> <mode>: count of selector-matched pods (any
# phase, including terminating) that do NOT yet carry ${MODE_LABEL}=<mode>. Read
# live so a deploy that lands mid-run is reflected in the decision. Prints 0 for an
# empty selector (matchExpressions), which is labeled/verified out of band.
pods_missing_mode() {
  local ns=$1 sel=$2 mode=$3
  [[ -z "$sel" ]] && { echo 0; return 0; }
  k get pods -n "$ns" -l "$sel" -o json 2>/dev/null \
    | jq -r --arg l "$MODE_LABEL" --arg v "$mode" \
        '[.items[] | select((.metadata.labels[$l] // "") != $v)] | length'
}

# enroll_with_restart <kind> <ns> <name> <id> <mode> <act>: the default >1-replica
# route. One combined template patch (mode label + restartedAt) triggers a single
# rolling update; then wait on rollout status. Deliberately restarts.
enroll_with_restart() {
  local kind=$1 ns=$2 name=$3 id=$4 mode=$5 act=$6
  if ! $DO_APPLY; then
    record PLAN "$id" "${act} on the template + restart; wait on rollout up to ${WAIT_TIMEOUT}s"
    return 0
  fi
  local patch
  patch=$(build_template_patch "$mode" true)
  if ! k patch "$kind" -n "$ns" "$name" --type merge -p "$patch" >/dev/null; then
    record FAIL "$id" "failed to patch template"
    return 0
  fi
  if k rollout status "$kind/$name" -n "$ns" --timeout="${WAIT_TIMEOUT}s" >/dev/null 2>&1; then
    record OK "$id" "enrolled (${act}); rolling restart complete, pods carry the label"
  else
    record WARN "$id" "enrolled (${act}) but rollout did not finish within ${WAIT_TIMEOUT}s; check: kubectl -n ${ns} rollout status ${kind}/${name}"
  fi
}

# ---------------------------------------------------------------------------
# Deployments
# ---------------------------------------------------------------------------

current_rs_name() { # <rs-list-json> <deploy-uid> <revision>
  jq -r --arg uid "$2" --arg rev "$3" '
    [.items[]
     | select(any(.metadata.ownerReferences[]?; (.controller == true) and (.uid == $uid)))
     | select(.metadata.annotations["deployment.kubernetes.io/revision"] == $rev)]
    | if length == 1 then .[0].metadata.name else empty end' <<<"$1"
}

# sync_deployment_templates_in_place <ns> <name> <id> <rs> <rs-all-json>: add the
# mode label to BOTH the current ReplicaSet template and the Deployment template
# while the Deployment is paused, so that on resume the controller re-adopts the
# existing ReplicaSet (its template now equals the Deployment's, ignoring the
# pod-template-hash) instead of starting a rollout. Records FAIL and returns 1 on
# any error; the caller returns on a non-zero.
#
# Note: there is deliberately no "did the controller create a different ReplicaSet"
# check here. That guard keyed on ReplicaSet identity/count, which a benign
# revision bump or history GC changes without restarting a single pod — so it fired
# false positives AND, by returning early, skipped the pod labeling. The reliable
# "did anything restart" signal is the running-pod UID diff in verify_owner, which
# the caller runs afterward.
sync_deployment_templates_in_place() {
  local ns=$1 name=$2 id=$3 rs=$4 rs_all=$5
  local rs_orig_mode patch
  # Original RS mode value so a failed Deployment patch can revert the RS before
  # resuming (a template mismatch on resume would trigger the very rollout we avoid).
  rs_orig_mode=$(jq -r --arg n "$rs" --arg l "$MODE_LABEL" \
    '[.items[] | select(.metadata.name == $n)][0].spec.template.metadata.labels[$l] // ""' <<<"$rs_all")
  patch=$(build_template_patch "$MODE" false)

  INFLIGHT="$id paused; recover with: kubectl -n $ns rollout resume deployment/$name"
  if ! k rollout pause -n "$ns" "deployment/$name" >/dev/null; then
    INFLIGHT=""; record FAIL "$id" "failed to pause"; return 1
  fi
  if ! k patch replicaset -n "$ns" "$rs" --type merge -p "$patch" >/dev/null; then
    k rollout resume -n "$ns" "deployment/$name" >/dev/null || true
    INFLIGHT=""; record FAIL "$id" "failed to patch ReplicaSet ${rs} (deployment resumed unchanged)"; return 1
  fi
  INFLIGHT="$id paused with ReplicaSet $rs patched; recover with: revert the ${MODE_LABEL} label on the RS template, then kubectl -n $ns rollout resume deployment/$name"
  if ! k patch deployment -n "$ns" "$name" --type merge -p "$patch" >/dev/null; then
    local revert
    revert=$(jq -cn --arg l "$MODE_LABEL" --arg v "$rs_orig_mode" \
      '{spec:{template:{metadata:{labels:{($l): (if $v == "" then null else $v end)}}}}}')
    if ! k patch replicaset -n "$ns" "$rs" --type merge -p "$revert" >/dev/null; then
      warn "$id: failed to revert ReplicaSet ${rs}; fix its template to match the Deployment before resuming"
      record FAIL "$id" "Deployment patch failed AND ReplicaSet revert failed; deployment left paused, resolve manually"
      return 1
    fi
    k rollout resume -n "$ns" "deployment/$name" >/dev/null || true
    INFLIGHT=""; record FAIL "$id" "failed to patch Deployment (ReplicaSet reverted, deployment resumed unchanged)"; return 1
  fi
  INFLIGHT="$id fully patched but still paused; recover with: kubectl -n $ns rollout resume deployment/$name"
  if ! k rollout resume -n "$ns" "deployment/$name" >/dev/null; then
    INFLIGHT=""; record FAIL "$id" "templates patched but resume failed; run: kubectl -n $ns rollout resume deployment/$name"; return 1
  fi
  INFLIGHT=""
  return 0
}

process_deployment() {
  local ns=$1 name=$2
  local id="deployment ${ns}/${name}"
  local djson
  djson=$(get_obj deployment "$ns" "$name" "$TMP_DIR/deployments.json")

  # Candidates are pre-filtered on the identity tuple, but apply mode re-reads live,
  # so re-check before doing anything.
  if ! jq -e "${JQ_QUAL_ARGS[@]}" "$QUALIFY_DEFS"' hastuple(.spec.template.metadata.labels // {})' <<<"$djson" >/dev/null; then
    record SKIP "$id" "no longer carries the full identity tuple"
    return 0
  fi

  local dmode
  dmode=$(jq -r --arg l "$MODE_LABEL" '.spec.template.metadata.labels[$l] // ""' <<<"$djson")

  # Template already enrolled at a *different* mode: only --remode changes it.
  if [[ -n "$dmode" && "$dmode" != "$MODE" ]] && ! $REMODE; then
    record WARN "$id" "already enrolled at ${MODE_LABEL}=${dmode}, not ${MODE}; left unchanged (re-run with --remode to change it)"
    return 0
  fi

  if jq -e '.spec.paused == true' <<<"$djson" >/dev/null; then
    record SKIP "$id" "deployment is paused; resolve, then re-run"
    return 0
  fi
  if ! jq -e '
      (.status.observedGeneration // 0) >= .metadata.generation
      and ((.spec.replicas // 1) == (.status.replicas // 0))
      and ((.spec.replicas // 1) == (.status.updatedReplicas // 0))
      and ((.spec.replicas // 1) == (.status.availableReplicas // 0))' <<<"$djson" >/dev/null; then
    record SKIP "$id" "rollout not settled; wait for it to finish, then re-run"
    return 0
  fi

  # Examine the current ReplicaSet and the pods too, not just the Deployment, so a
  # workload left half-enrolled by an earlier run (template labeled but its current
  # ReplicaSet or its pods not) is detected as broken and repaired, not skipped.
  local uid rev rs_all rs rsmode sel missing
  uid=$(jq -r '.metadata.uid' <<<"$djson")
  rev=$(jq -r '.metadata.annotations["deployment.kubernetes.io/revision"] // empty' <<<"$djson")
  rs_all=$(rs_list_for "$ns")
  rs=$(current_rs_name "$rs_all" "$uid" "$rev")
  if [[ -z "$rs" ]]; then
    record SKIP "$id" "could not identify a unique current ReplicaSet (revision ${rev:-<none>})"
    return 0
  fi
  rsmode=$(jq -r --arg n "$rs" --arg l "$MODE_LABEL" \
    '[.items[] | select(.metadata.name == $n)][0].spec.template.metadata.labels[$l] // ""' <<<"$rs_all")
  sel=$(selector_of "$djson")
  missing=$(pods_missing_mode "$ns" "$sel" "$MODE")

  local need_deploy=false need_rs=false
  [[ "$dmode"  != "$MODE" ]] && need_deploy=true
  [[ "$rsmode" != "$MODE" ]] && need_rs=true

  # Fully converged: template, current ReplicaSet, and every pod already at $MODE.
  if ! $need_deploy && ! $need_rs && [[ "${missing:-0}" -eq 0 ]]; then
    echo x >>"$CONVERGED_FILE"
    return 0
  fi

  local replicas act
  replicas=$(jq -r '.spec.replicas // 1' <<<"$djson")
  act=$(mode_action "$djson")

  if ! $DO_APPLY; then
    local plan="label ${missing:-0} unlabeled pod(s)"
    if $need_deploy || $need_rs; then
      if ! $FORCE_NO_RESTART && (( replicas > 1 )); then
        plan="${act} on template + rolling restart; then ${plan}"
      else
        plan="${act} on ReplicaSet ${rs} + Deployment templates in place (adopt, no rollout); then ${plan}"
      fi
    fi
    record PLAN "$id" "${plan}; verify the running-pod set is unchanged"
    return 0
  fi

  # Label the current pods FIRST: it makes them visible to the operator's label-
  # scoped cache, so if any are deleted next (by the rolling restart below, or later)
  # the operator can run their finalizer instead of leaking it. No-op when already
  # labeled.
  patch_pods_mode "$ns" "$sel" "$MODE" "$id"

  # Templates need the label: multi-replica without --no-restart takes the clean
  # rolling-restart route (its new pods are born labeled); everything else syncs the
  # ReplicaSet + Deployment templates in place with no restart, then confirms no
  # rollout started — the only case in which the running-pod set can change, so the
  # only case that warrants the settle-and-verify wait.
  if $need_deploy || $need_rs; then
    if ! $FORCE_NO_RESTART && (( replicas > 1 )); then
      enroll_with_restart deployment "$ns" "$name" "$id" "$MODE" "$act"
      return 0
    fi
    local before=""
    [[ -n "$sel" ]] && before=$(snapshot_uids "$ns" "$sel")
    sync_deployment_templates_in_place "$ns" "$name" "$id" "$rs" "$rs_all" || return 0
    sleep "$SETTLE"
    verify_owner "$id" "$ns" "$sel" "$before" "enrolled in place (${act}); ReplicaSet ${rs} adopted, pods labeled"
    return 0
  fi

  # Pods-only repair (templates already at $MODE): labeling is pure metadata — no
  # restart, nothing to settle or verify.
  record OK "$id" "repaired in place (${act}); ${missing} pod(s) labeled, template already at ${MODE}, no restart"
}

# ---------------------------------------------------------------------------
# StatefulSets
# ---------------------------------------------------------------------------

sts_has_new_revision() { # <ns> <name> <old-revision>
  local j
  j=$(k get statefulset -n "$1" "$2" -o json)
  jq -e --arg old "$3" \
    '((.status.observedGeneration // 0) >= .metadata.generation) and ((.status.updateRevision // "") != $old)' \
    <<<"$j" >/dev/null
}

process_statefulset() {
  local ns=$1 name=$2
  local id="statefulset ${ns}/${name}"
  local j
  j=$(get_obj statefulset "$ns" "$name" "$TMP_DIR/statefulsets.json")

  owner_qualifies "$id" "$j" || return 0

  local strategy replicas act
  strategy=$(jq -r '.spec.updateStrategy.type // "RollingUpdate"' <<<"$j")
  replicas=$(jq -r '.spec.replicas // 1' <<<"$j")
  act=$(mode_action "$j")

  if [[ "$strategy" == "OnDelete" ]]; then
    if ! jq -e '(.status.observedGeneration // 0) >= .metadata.generation' <<<"$j" >/dev/null; then
      record SKIP "$id" "controller has not observed the latest generation; wait, then re-run"
      return 0
    fi
  elif ! jq -e '
      (.status.observedGeneration // 0) >= .metadata.generation
      and ((.status.currentRevision // "") == (.status.updateRevision // ""))
      and ((.spec.replicas // 1) == (.status.readyReplicas // 0))' <<<"$j" >/dev/null; then
    record SKIP "$id" "update not settled; wait for it to finish, then re-run"
    return 0
  fi

  if ! $FORCE_NO_RESTART && [[ "$strategy" != "OnDelete" ]] && (( replicas > 1 )); then
    enroll_with_restart statefulset "$ns" "$name" "$id" "$MODE" "$act"
    return 0
  fi

  local sel patch
  sel=$(selector_of "$j")
  patch=$(build_template_patch "$MODE" false)

  # OnDelete (any replica count): patching the template never touches running
  # pods, so just label the template and the live pods in place.
  if [[ "$strategy" == "OnDelete" ]]; then
    if ! $DO_APPLY; then
      record PLAN "$id" "OnDelete: ${act} on template; label live pods in place; no restart"
      return 0
    fi
    if ! k patch statefulset -n "$ns" "$name" --type merge -p "$patch" >/dev/null; then
      record FAIL "$id" "failed to patch template"
      return 0
    fi
    patch_pods_mode "$ns" "$sel" "$MODE" "$id"
    record OK "$id" "enrolled (${act}); OnDelete template labeled, live pods labeled, no restart"
    return 0
  fi

  # RollingUpdate no-restart route (<= 1 replica, or --no-restart): raise partition
  # to spec.replicas, patch the template, relabel pods to the new
  # controller-revision-hash, restore partition, then label live pods.
  local partition oldrev
  partition=$(jq -r '.spec.updateStrategy.rollingUpdate.partition // 0' <<<"$j")
  oldrev=$(jq -r '.status.updateRevision // ""' <<<"$j")

  if ! $DO_APPLY; then
    record PLAN "$id" "no restart: raise partition ${partition} -> ${replicas}; ${act} on template; relabel ${replicas} pod(s) to the new controller-revision-hash; label live pod(s); restore partition ${partition}; verify"
    return 0
  fi

  local before=""
  if [[ -n "$sel" ]]; then
    before=$(snapshot_uids "$ns" "$sel")
  fi

  local part_raise part_restore
  part_raise=$(jq -cn --argjson p "$replicas" '{spec:{updateStrategy:{rollingUpdate:{partition:$p}}}}')
  part_restore=$(jq -cn --argjson p "$partition" '{spec:{updateStrategy:{rollingUpdate:{partition:$p}}}}')

  INFLIGHT="$id partition raised to $replicas; recover with: kubectl -n $ns patch statefulset $name --type merge -p '$part_restore' (only after pods carry the current updateRevision label)"
  if ! k patch statefulset -n "$ns" "$name" --type merge -p "$part_raise" >/dev/null; then
    INFLIGHT=""
    record FAIL "$id" "failed to raise partition"
    return 0
  fi

  if ! k patch statefulset -n "$ns" "$name" --type merge -p "$patch" >/dev/null; then
    k patch statefulset -n "$ns" "$name" --type merge -p "$part_restore" >/dev/null || true
    INFLIGHT=""
    record FAIL "$id" "failed to patch template (partition restored)"
    return 0
  fi

  if ! wait_until "$WAIT_TIMEOUT" sts_has_new_revision "$ns" "$name" "$oldrev"; then
    record FAIL "$id" "timed out waiting for the new updateRevision; partition left at ${replicas} so nothing restarts; re-run once the controller catches up"
    INFLIGHT=""
    return 0
  fi

  local newrev
  newrev=$(k get statefulset -n "$ns" "$name" -o jsonpath='{.status.updateRevision}')

  if [[ -z "$sel" ]]; then
    record FAIL "$id" "template labeled but selector uses matchExpressions; relabel pods to controller-revision-hash=${newrev} manually, then restore partition to ${partition}"
    INFLIGHT=""
    return 0
  fi
  if ! k label pods -n "$ns" -l "$sel" --overwrite "controller-revision-hash=${newrev}" >/dev/null; then
    record FAIL "$id" "failed to relabel pods; partition left at ${replicas} so nothing restarts; relabel to controller-revision-hash=${newrev}, then restore partition to ${partition}"
    INFLIGHT=""
    return 0
  fi

  if ! k patch statefulset -n "$ns" "$name" --type merge -p "$part_restore" >/dev/null; then
    record FAIL "$id" "pods relabeled but partition restore failed; run: kubectl -n $ns patch statefulset $name --type merge -p '$part_restore'"
    INFLIGHT=""
    return 0
  fi
  INFLIGHT=""

  patch_pods_mode "$ns" "$sel" "$MODE" "$id"
  sleep "$SETTLE"
  verify_owner "$id" "$ns" "$sel" "$before" "enrolled in place (${act}); pods relabeled to revision ${newrev} and labeled, no restart"
}

# ---------------------------------------------------------------------------
# DaemonSets
# ---------------------------------------------------------------------------

ds_generation_observed() { # <ns> <name>
  local j
  j=$(k get daemonset -n "$1" "$2" -o json)
  jq -e '(.status.observedGeneration // 0) >= .metadata.generation' <<<"$j" >/dev/null
}

process_daemonset() {
  local ns=$1 name=$2
  local id="daemonset ${ns}/${name}"
  local j
  j=$(get_obj daemonset "$ns" "$name" "$TMP_DIR/daemonsets.json")

  owner_qualifies "$id" "$j" || return 0

  if ! jq -e '
      (.status.observedGeneration // 0) >= .metadata.generation
      and ((.status.updatedNumberScheduled // 0) == (.status.desiredNumberScheduled // 0))
      and ((.status.numberUnavailable // 0) == 0)' <<<"$j" >/dev/null; then
    record SKIP "$id" "update not settled; wait for it to finish, then re-run"
    return 0
  fi

  local strategy scheduled act
  strategy=$(jq -r '.spec.updateStrategy.type // "RollingUpdate"' <<<"$j")
  # A DaemonSet's "replica count" is how many nodes it is scheduled on.
  scheduled=$(jq -r '.status.desiredNumberScheduled // 0' <<<"$j")
  act=$(mode_action "$j")

  if ! $FORCE_NO_RESTART && [[ "$strategy" != "OnDelete" ]] && (( scheduled > 1 )); then
    enroll_with_restart daemonset "$ns" "$name" "$id" "$MODE" "$act"
    return 0
  fi

  local sel patch
  sel=$(selector_of "$j")
  patch=$(build_template_patch "$MODE" false)

  # OnDelete (any node count): label the template and the live pods in place.
  if [[ "$strategy" == "OnDelete" ]]; then
    if ! $DO_APPLY; then
      record PLAN "$id" "OnDelete: ${act} on template; label live pods in place; no restart"
      return 0
    fi
    if ! k patch daemonset -n "$ns" "$name" --type merge -p "$patch" >/dev/null; then
      record FAIL "$id" "failed to patch template"
      return 0
    fi
    patch_pods_mode "$ns" "$sel" "$MODE" "$id"
    record OK "$id" "enrolled (${act}); OnDelete template labeled, live pods labeled, no restart"
    return 0
  fi

  # RollingUpdate no-restart route (single node, or --no-restart): temporarily
  # switch to OnDelete, patch the template, relabel the pod(s) to the new
  # controller-revision-hash, restore the strategy, then label the live pod(s).
  local orig_strategy uid
  orig_strategy=$(jq -c '.spec.updateStrategy // {type:"RollingUpdate"}' <<<"$j")
  uid=$(jq -r '.metadata.uid' <<<"$j")

  if ! $DO_APPLY; then
    record PLAN "$id" "no restart: switch strategy to OnDelete; ${act} on template; relabel pod(s) to the new controller-revision-hash; label live pod(s); restore strategy; verify"
    return 0
  fi

  local before=""
  if [[ -n "$sel" ]]; then
    before=$(snapshot_uids "$ns" "$sel")
  fi

  local strat_restore
  strat_restore=$(jq -cn --argjson s "$orig_strategy" '{spec:{updateStrategy:$s}}')

  INFLIGHT="$id strategy switched to OnDelete; recover with: kubectl -n $ns patch daemonset $name --type merge -p '$strat_restore' (only after pods carry the current controller-revision-hash label)"
  if ! k patch daemonset -n "$ns" "$name" --type merge -p '{"spec":{"updateStrategy":{"type":"OnDelete","rollingUpdate":null}}}' >/dev/null; then
    INFLIGHT=""
    record FAIL "$id" "failed to switch strategy to OnDelete"
    return 0
  fi

  if ! k patch daemonset -n "$ns" "$name" --type merge -p "$patch" >/dev/null; then
    k patch daemonset -n "$ns" "$name" --type merge -p "$strat_restore" >/dev/null || true
    INFLIGHT=""
    record FAIL "$id" "failed to patch template (strategy restored)"
    return 0
  fi

  if ! wait_until "$WAIT_TIMEOUT" ds_generation_observed "$ns" "$name"; then
    record FAIL "$id" "timed out waiting for the controller to observe the template change; strategy left OnDelete so nothing restarts; re-run once it catches up"
    INFLIGHT=""
    return 0
  fi

  # The newest ControllerRevision carries the hash pods must be labeled with (DS
  # pods use the bare hash, unlike StatefulSets which use the full revision name).
  local newhash
  newhash=$(k get controllerrevisions -n "$ns" -o json | jq -r --arg uid "$uid" '
    [.items[] | select(any(.metadata.ownerReferences[]?; .uid == $uid))]
    | if length == 0 then "" else (max_by(.revision).metadata.labels["controller-revision-hash"] // "") end')
  if [[ -z "$newhash" ]]; then
    record FAIL "$id" "could not determine the new controller-revision-hash; strategy left OnDelete so nothing restarts; resolve manually"
    INFLIGHT=""
    return 0
  fi

  if [[ -z "$sel" ]]; then
    record FAIL "$id" "template labeled but selector uses matchExpressions; relabel pods to controller-revision-hash=${newhash} manually, then restore strategy: ${strat_restore}"
    INFLIGHT=""
    return 0
  fi
  if ! k label pods -n "$ns" -l "$sel" --overwrite "controller-revision-hash=${newhash}" >/dev/null; then
    record FAIL "$id" "failed to relabel pods; strategy left OnDelete so nothing restarts; relabel to controller-revision-hash=${newhash}, then restore strategy: ${strat_restore}"
    INFLIGHT=""
    return 0
  fi

  if ! k patch daemonset -n "$ns" "$name" --type merge -p "$strat_restore" >/dev/null; then
    record FAIL "$id" "pods relabeled but strategy restore failed; run: kubectl -n $ns patch daemonset $name --type merge -p '$strat_restore'"
    INFLIGHT=""
    return 0
  fi
  INFLIGHT=""

  patch_pods_mode "$ns" "$sel" "$MODE" "$id"
  sleep "$SETTLE"
  verify_owner "$id" "$ns" "$sel" "$before" "enrolled in place (${act}); pod(s) relabeled to hash ${newhash} and labeled, no restart"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

# skip_ignored <name>: true when the workload name matches the ignore regex.
skip_ignored() {
  [[ -n "$IGNORE_REGEX" ]] && [[ "$1" =~ $IGNORE_REGEX ]]
}

main() {
  if $DO_APPLY; then
    info "=== APPLY MODE [mode=${MODE}]: changes will be made ($( [[ -n "$NAMESPACE" ]] && echo "namespace $NAMESPACE" || echo "all namespaces" )) ==="
  else
    info "=== DRY-RUN [mode=${MODE}]: no changes will be made; re-run with --apply to execute ==="
  fi
  info "Identity tuple (all required, non-empty): $(IFS=', '; echo "${IDENTITY_LABELS[*]}") [source: ${IDENTITY_SOURCE}]"
  [[ -n "$IGNORE_REGEX" ]] && info "Ignoring workload names matching: /${IGNORE_REGEX}/"
  $FORCE_NO_RESTART && info "Route: no-restart (hotfix in place) for all workloads, including multi-replica."
  $REMODE && info "Re-mode: workloads already at a different mode will be changed to ${MODE}."
  info ""
  info "Scanning for qualifying workloads ..."

  k get deployments "${NS_ARGS[@]}" -o json >"$TMP_DIR/deployments.json"
  k get statefulsets "${NS_ARGS[@]}" -o json >"$TMP_DIR/statefulsets.json"
  k get daemonsets "${NS_ARGS[@]}" -o json >"$TMP_DIR/daemonsets.json"
  k get replicasets "${NS_ARGS[@]}" -o json >"$TMP_DIR/replicasets.json"

  local owner_filter='.items[] | select('"$QUAL_FN"'(.spec.template.metadata.labels // {})) | [.metadata.namespace, .metadata.name] | @tsv'
  # Deployments are selected on the identity tuple ALONE, not on QUAL_FN, so that a
  # deployment already carrying the mode label is still handed to process_deployment.
  # It examines the current ReplicaSet and the pods and repairs a half-enrolled state
  # (template labeled but pods not); a genuinely fully-enrolled one is a quiet no-op.
  local deploy_filter='.items[] | select(hastuple(.spec.template.metadata.labels // {})) | [.metadata.namespace, .metadata.name] | @tsv'
  local deploys sts ds
  deploys=$(jq -r "${JQ_QUAL_ARGS[@]}" "$QUALIFY_DEFS$deploy_filter" "$TMP_DIR/deployments.json")
  sts=$(jq -r "${JQ_QUAL_ARGS[@]}" "$QUALIFY_DEFS$owner_filter" "$TMP_DIR/statefulsets.json")
  ds=$(jq -r "${JQ_QUAL_ARGS[@]}" "$QUALIFY_DEFS$owner_filter" "$TMP_DIR/daemonsets.json")

  count_lines() { if [[ -z "$1" ]]; then echo 0; else printf '%s\n' "$1" | grep -c .; fi; }
  info "Found (before ignore filter): $(count_lines "$deploys") deployment(s), $(count_lines "$sts") statefulset(s), $(count_lines "$ds") daemonset(s)"
  info ""

  # Already-enrolled workloads (identity tuple + a mode label) that are NOT in the
  # target set. Reported so a re-run is transparent: a one-line count for those
  # already at the requested mode (pure no-op). A workload at a *different* mode is
  # a target under --remode (handled in the processing loops below), so it is only
  # flagged here — WARN, left unchanged — in the default mode, where the tool adds
  # enrollment but never silently re-modes an already-managed workload.
  local enrolled_filter='.items[]
    | (.spec.template.metadata.labels // {}) as $l
    | select(hastuple($l) and ne($l; $mlbl))
    | [$kind, .metadata.namespace, .metadata.name, modeval($l)] | @tsv'
  # Deployments are intentionally omitted here: they are all handed to
  # process_deployment (see deploy_filter above), which reports their own
  # already-enrolled / different-mode / fully-converged status.
  local enrolled ekind ens ename ecur same_count=0
  enrolled=$(
    jq -r "${JQ_QUAL_ARGS[@]}" --arg kind statefulset "$QUALIFY_DEFS$enrolled_filter" "$TMP_DIR/statefulsets.json"
    jq -r "${JQ_QUAL_ARGS[@]}" --arg kind daemonset   "$QUALIFY_DEFS$enrolled_filter" "$TMP_DIR/daemonsets.json"
  )
  while IFS=$'\t' read -r ekind ens ename ecur; do
    [[ -z "$ens" ]] && continue
    skip_ignored "$ename" && continue
    if [[ "$ecur" == "$MODE" ]]; then
      same_count=$((same_count + 1))
    elif ! $REMODE; then
      record WARN "${ekind} ${ens}/${ename}" "already enrolled at ${MODE_LABEL}=${ecur}, not ${MODE}; left unchanged (re-run with --remode to change it)"
    fi
  done <<<"$enrolled"
  if (( same_count > 0 )); then
    record NOTE "already enrolled" "${same_count} workload(s) already carry ${MODE_LABEL}=${MODE}; nothing to do (no patch, no restart)"
  fi

  local ns name

  while IFS=$'\t' read -r ns name; do
    [[ -z "$ns" ]] && continue
    if skip_ignored "$name"; then record SKIP "deployment ${ns}/${name}" "matches --ignore /${IGNORE_REGEX}/"; continue; fi
    process_deployment "$ns" "$name"
  done <<<"$deploys"

  while IFS=$'\t' read -r ns name; do
    [[ -z "$ns" ]] && continue
    if skip_ignored "$name"; then record SKIP "statefulset ${ns}/${name}" "matches --ignore /${IGNORE_REGEX}/"; continue; fi
    process_statefulset "$ns" "$name"
  done <<<"$sts"

  while IFS=$'\t' read -r ns name; do
    [[ -z "$ns" ]] && continue
    if skip_ignored "$name"; then record SKIP "daemonset ${ns}/${name}" "matches --ignore /${IGNORE_REGEX}/"; continue; fi
    process_daemonset "$ns" "$name"
  done <<<"$ds"

  local n_converged
  n_converged=$(grep -c . "$CONVERGED_FILE" || true)
  if (( n_converged > 0 )); then
    record NOTE "already enrolled" "${n_converged} deployment(s) already fully enrolled at ${MODE_LABEL}=${MODE} (template, ReplicaSet, and all pods); nothing to do"
  fi

  local n_ok n_fail n_skip n_warn
  n_ok=$(grep -c $'^OK\t' "$RESULTS_FILE" || true)
  n_fail=$(grep -c $'^FAIL\t' "$RESULTS_FILE" || true)
  n_skip=$(grep -c $'^SKIP\t' "$RESULTS_FILE" || true)
  n_warn=$(grep -c $'^WARN\t' "$RESULTS_FILE" || true)

  info ""
  info "Summary: ${n_ok} enrolled, ${n_fail} failed, ${n_warn} warning(s), ${n_skip} skipped."
  if [[ "$n_fail" != "0" ]]; then
    exit 2
  fi
}

main
