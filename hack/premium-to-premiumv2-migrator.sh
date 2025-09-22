#!/bin/bash

set -euo pipefail
IFS=$'\n\t'

# Record script start (run-level metadata)
SCRIPT_START_TS="$(date +'%Y-%m-%dT%H:%M:%S')"
SCRIPT_START_EPOCH="$(date +%s)"

# ---------------- Configuration ----------------
MIGRATION_LABEL="${MIGRATION_LABEL:-disk.csi.azure.com/pv2migration=true}"
NAMESPACE="${NAMESPACE:-}"
MAX_PVCS="${MAX_PVCS:-50}"
SNAPSHOT_CLASS="${SNAPSHOT_CLASS:-csi-azuredisk-vsc}"
POLL_INTERVAL="${POLL_INTERVAL:-120}"
MONITOR_TIMEOUT_MINUTES="${MONITOR_TIMEOUT_MINUTES:-120}"

# Snapshot staleness controls
# SNAPSHOT_MAX_AGE_SECONDS: if >0, consider a snapshot stale when older than this many seconds (based on creationTimestamp)
# SNAPSHOT_RECREATE_ON_STALE: if true, stale snapshots (and only if detected) are deleted & recreated
SNAPSHOT_MAX_AGE_SECONDS="${SNAPSHOT_MAX_AGE_SECONDS:-7200}"
SNAPSHOT_RECREATE_ON_STALE="${SNAPSHOT_RECREATE_ON_STALE:-false}"

# Audit logging controls
AUDIT_ENABLE="${AUDIT_ENABLE:-true}"
# If set, audit lines also written to this file (append mode)
AUDIT_LOG_FILE="${AUDIT_LOG_FILE:-}"

# Internal audit storage (pipe-delimited fields)
declare -a AUDIT_LINES=()

# audit_add(kind, name, namespace, action, revert, extra)
audit_add() {
  [[ "$AUDIT_ENABLE" != "true" ]] && return 0
  local kind="$1" name="$2" namespace="$3" action="$4" revert="$5" extra="$6"
  local timestamp
  timestamp="$(ts)"
  AUDIT_LINES+=("${timestamp}|${action}|${kind}|${namespace}|${name}|${revert}|${extra}")
  if [[ -n "$AUDIT_LOG_FILE" ]]; then
    printf '%s\n' "${AUDIT_LINES[-1]}" >>"$AUDIT_LOG_FILE" 2>/dev/null || true
  fi
}

MIG_SUFFIX="${MIG_SUFFIX:-csi}"
WAIT_FOR_WORKLOAD="${WAIT_FOR_WORKLOAD:-true}"
WORKLOAD_DETACH_TIMEOUT_MINUTES="${WORKLOAD_DETACH_TIMEOUT_MINUTES:-0}"

# New: label key + value constants for provenance
CREATED_BY_LABEL_KEY="${CREATED_BY_LABEL_KEY:-disk.csi.azure.com/created-by}"
MIGRATION_TOOL_ID="${MIGRATION_TOOL_ID:-azuredisk-pv1-to-pv2-migrator}"

# New: migration completion label (was hard-coded in JSONPath and label commands)
MIGRATION_DONE_LABEL_KEY="${MIGRATION_DONE_LABEL_KEY:-disk.csi.azure.com/migration-done}"
MIGRATION_DONE_LABEL_VALUE="${MIGRATION_DONE_LABEL_VALUE:-true}"

# NEW: migration in-progress fallback labeling
MIGRATION_IN_PROGRESS_LABEL_KEY="${MIGRATION_IN_PROGRESS_LABEL_KEY:-disk.csi.azure.com/migration-in-progress}"
MIGRATION_IN_PROGRESS_LABEL_VALUE="${MIGRATION_IN_PROGRESS_LABEL_VALUE:-true}"
# After this many minutes without events + label, force-apply label (0 disables proactive mid-loop forcing)
MIGRATION_FORCE_INPROGRESS_AFTER_MINUTES="${MIGRATION_FORCE_INPROGRESS_AFTER_MINUTES:-10}"

# kubectl retry tuning (already present)
KUBECTL_MAX_RETRIES="${KUBECTL_MAX_RETRIES:-5}"
KUBECTL_RETRY_BASE_DELAY="${KUBECTL_RETRY_BASE_DELAY:-2}"
KUBECTL_RETRY_MAX_DELAY="${KUBECTL_RETRY_MAX_DELAY:-30}"
KUBECTL_TRANSIENT_REGEX="${KUBECTL_TRANSIENT_REGEX:-(connection refused|i/o timeout|timeout exceeded|TLS handshake timeout|context deadline exceeded|Service Unavailable|Too Many Requests|EOF|transport is closing|Internal error|no route to host|Connection reset by peer)}"

# ---------------- Logging Helpers ----------------
ts()   { date +'%Y-%m-%dT%H:%M:%S'; }
info() { echo "$(ts) [INFO] $*"; }
warn() { echo "$(ts) [WARN] $*" >&2; }
err()  { echo "$(ts) [ERROR] $*" >&2; }
ok()   { echo "$(ts) [OK] $*"; }

# Format seconds -> HhMmSs (compact)
human_duration() {
  local total=$1
  local h=$(( total / 3600 ))
  local m=$(( (total % 3600) / 60 ))
  local s=$(( total % 60 ))
  if (( h > 0 )); then
    printf '%dh%02dm%02ds' "$h" "$m" "$s"
  elif (( m > 0 )); then
    printf '%dm%02ds' "$m" "$s"
  else
    printf '%ds' "$s"
  fi
}

# kubectl with retry (already present)
kubectl_retry() {
  local attempt=1
  local max="$KUBECTL_MAX_RETRIES"
  local base="$KUBECTL_RETRY_BASE_DELAY"
  local max_delay="$KUBECTL_RETRY_MAX_DELAY"
  local pattern="$KUBECTL_TRANSIENT_REGEX"
  local output rc sleep_time
  while true; do
    set +e
    output=$(kubectl "$@" 2>&1)
    rc=$?
    set -e
    if [[ $rc -eq 0 ]]; then
      printf '%s' "$output"
      return 0
    fi
    if ! grep -qiE "$pattern" <<<"$output"; then
      echo "$output" >&2
      return $rc
    fi
    if (( attempt >= max )); then
      warn "kubectl retry exhausted ($attempt attempts): kubectl $*"
      echo "$output" >&2
      return $rc
    fi
    sleep_time=$(( base * 2 ** (attempt-1) ))
    (( sleep_time > max_delay )) && sleep_time=$max_delay
    warn "kubectl transient failure (attempt $attempt/$max) -> retrying in ${sleep_time}s: $(head -n1 <<<"$output")"
    sleep "$sleep_time"
    attempt=$(( attempt + 1 ))
  done
}

kget() { kubectl_retry get "$@"; }

# ### NEW: Reliable apply with retry for heredoc YAML
kapply_retry() {
  # Usage: kapply_retry <<EOF
  # <yaml>
  # EOF
  local tmp rc attempt=1
  local max="$KUBECTL_MAX_RETRIES"
  local base="$KUBECTL_RETRY_BASE_DELAY"
  local max_delay="$KUBECTL_RETRY_MAX_DELAY"
  local pattern="$KUBECTL_TRANSIENT_REGEX"
  tmp="$(mktemp)"
  cat >"$tmp"

  while true; do
    set +e
    output=$(kubectl apply -f "$tmp" 2>&1)
    rc=$?
    set -e
    if [[ $rc -eq 0 ]]; then
      printf '%s\n' "$output"
      rm -f "$tmp"
      return 0
    fi
    if ! grep -qiE "$pattern" <<<"$output"; then
      echo "$output" >&2
      rm -f "$tmp"
      return $rc
    fi
    if (( attempt >= max )); then
      warn "kubectl apply retry exhausted ($attempt attempts)"
      local first_line
      first_line=$(head -n1 <<<"$output" | sed 's/"/'\''/g')
      echo "MIG_APPLY_FAILURE kind=$(grep -m1 '^kind:' "$tmp" | awk '{print $2}') name=$(grep -m1 '^  name:' "$tmp" | awk '{print $2}') attempts=$attempt reason=\"$first_line\""
      echo "$output" >&2
      rm -f "$tmp"
      return $rc
    fi
    sleep_time=$(( base * 2 ** (attempt-1) ))
    (( sleep_time > max_delay )) && sleep_time=$max_delay
    warn "kubectl apply transient failure (attempt $attempt/$max) -> retry in ${sleep_time}s: $(head -n1 <<<"$output")"
    sleep "$sleep_time"
    attempt=$(( attempt + 1 ))
  done
}

# ---------------- Name Helper Functions ----------------
name_csi_pv()  { local pv="$1"; echo "${pv}-${MIG_SUFFIX}"; }
name_csi_pvc() { local pvc="$1"; echo "${pvc}-${MIG_SUFFIX}"; }
name_snapshot(){ local pv="$1"; echo "ss-$(name_csi_pv "$pv")"; }
name_pv2_pvc() { local pvc="$1"; echo "${pvc}-${MIG_SUFFIX}-pv2"; }

# ---------------- Preflight ----------------
for bin in kubectl jq; do
  command -v "$bin" >/dev/null 2>&1 || { err "Required command '$bin' not found in PATH"; exit 1; }
done

if [[ -n "$NAMESPACE" ]] && ! kubectl_retry get namespace "$NAMESPACE" >/dev/null 2>&1; then
  err "Namespace '$NAMESPACE' does not exist"
  exit 1
fi

if [[ -z "$MIG_SUFFIX" ]]; then
  err "Suffix MIG_SUFFIX cannot be empty"; exit 1
fi
if [[ ! "$MIG_SUFFIX" =~ ^[a-zA-Z0-9-]{1,20}$ ]]; then
  err "Invalid MIG_SUFFIX '$MIG_SUFFIX' (allowed: alphanum + dash, length <=20)"; exit 1
fi

info "Migration label: $MIGRATION_LABEL"
info "Target snapshot class: $SNAPSHOT_CLASS"
info "Max PVCs: $MAX_PVCS  MIG_SUFFIX=$MIG_SUFFIX  WAIT_FOR_WORKLOAD=$WAIT_FOR_WORKLOAD"
info "kubectl retry: max=$KUBECTL_MAX_RETRIES baseDelay=${KUBECTL_RETRY_BASE_DELAY}s maxDelay=${KUBECTL_RETRY_MAX_DELAY}s"

# ---------------- RBAC Validation (Preflight) ----------------
RBAC_CHECK="${RBAC_CHECK:-true}"
RBAC_FAIL_FAST="${RBAC_FAIL_FAST:-true}"
RBAC_EXTRA_VERBOSE="${RBAC_EXTRA_VERBOSE:-false}"

check_rbac() {
  [[ "$RBAC_CHECK" != "true" ]] && { info "RBAC preflight disabled (RBAC_CHECK=false)"; return 0; }

  info "Performing RBAC preflight permission checks..."

  # Cluster-scoped required verbs/resources
  local cluster_checks=(
    "get persistentvolumes"
    "create persistentvolumes"
    "patch persistentvolumes"
    "get storageclasses"
    "create storageclasses"
    "get volumesnapshotclasses.snapshot.storage.k8s.io"
    "create volumesnapshotclasses.snapshot.storage.k8s.io"
    "get volumeattachments.storage.k8s.io"
  )

  # If scanning all namespaces (NAMESPACE empty), need cluster-wide pvc list to enumerate
  if [[ -z "${NAMESPACE}" ]]; then
    cluster_checks+=("list persistentvolumeclaims")
  fi

  # Namespaced required (applies per namespace; when scanning all namespaces we only test generic ability)
  local ns_checks=(
    "get persistentvolumeclaims"
    "list persistentvolumeclaims"
    "create persistentvolumeclaims"
    "patch persistentvolumeclaims"
    "get volumesnapshots.snapshot.storage.k8s.io"
    "create volumesnapshots.snapshot.storage.k8s.io"
    "get events"
  )
  if [[ "$WAIT_FOR_WORKLOAD" == "true" ]]; then
    ns_checks+=("get pods")
  fi

  local failures=()
  local passed=0
  local total=0

  rbac_can() {
    local verb="$1"
    local res="$2"
    shift 2
    if kubectl auth can-i "$verb" "$res" "$@" >/dev/null 2>&1; then
      [[ "$RBAC_EXTRA_VERBOSE" == "true" ]] && info "RBAC OK: $verb $res $*"
      return 0
    else
      return 1
    fi
  }

  # Cluster-scoped checks
  for entry in "${cluster_checks[@]}"; do
    total=$((total+1))
    read -r verb res <<<"$entry"
    if rbac_can "$verb" "$res"; then
      passed=$((passed+1))
    else
      failures+=("cluster: $verb $res")
      warn "RBAC missing: cluster: $verb $res"
      [[ "$RBAC_FAIL_FAST" == "true" ]] && { err "RBAC fail-fast triggered."; return 1; }
    fi
  done

  # Namespaced checks
  if [[ -n "${NAMESPACE}" ]]; then
    for entry in "${ns_checks[@]}"; do
      total=$((total+1))
      read -r verb res <<<"$entry"
      if rbac_can "$verb" "$res" -n "$NAMESPACE"; then
        passed=$((passed+1))
      else
        failures+=("namespace:${NAMESPACE}: $verb $res")
        warn "RBAC missing: namespace:${NAMESPACE}: $verb $res"
        [[ "$RBAC_FAIL_FAST" == "true" ]] && { err "RBAC fail-fast triggered."; return 1; }
      fi
    done
  else
    # Generic single namespace test (without specifying -n) not meaningful; we just report expectation
    info "Note: Running cluster-wide. Ensure these verbs are granted in every target namespace:"
    printf '  %s\n' "${ns_checks[@]}"
    # Optionally we could sample a namespace; keeping it simple.
  fi

  if (( ${#failures[@]} > 0 )); then
    err "RBAC preflight failed: ${#failures[@]} missing permission(s)."
    printf 'Missing:\n'; printf '  %s\n' "${failures[@]}"
    return 1
  fi

  ok "RBAC preflight success ($passed/$total cluster/namespace checks passed)."
  return 0
}

# Invoke RBAC check early
check_rbac || { err "Aborting due to insufficient permissions."; exit 1; }

# ---------------- Functions (only showing changed lines) ----------------

# --- ADD: conflict detection function (place near other function defs) ---
ensure_no_foreign_conflicts() {
  info "Checking for pre-existing conflicting objects (PVC/PV/VolumeSnapshot/VolumeSnapshotClass) not created by this tool..."

  local conflicts=()

  # 1. VolumeSnapshotClass conflict (if exists but not ours)
  if kubectl get volumesnapshotclass "$SNAPSHOT_CLASS" >/dev/null 2>&1; then
    if ! kubectl get volumesnapshotclass "$SNAPSHOT_CLASS" -o json \
        | jq -e --arg k "$CREATED_BY_LABEL_KEY" --arg v "$MIGRATION_TOOL_ID" '.metadata.labels[$k]==$v' >/dev/null; then
      conflicts+=("VolumeSnapshotClass/$SNAPSHOT_CLASS (exists without ${CREATED_BY_LABEL_KEY}=${MIGRATION_TOOL_ID})")
    fi
  fi

  # 2. Per-PVC derived names
  for ENTRY in "${MIG_PVCS[@]}"; do
    local ns="${ENTRY%%|*}"
    local pvc="${ENTRY##*|}"

    # PV2 PVC
    local pv2_pvc
    pv2_pvc="$(name_pv2_pvc "$pvc")"
    if kubectl get pvc "$pv2_pvc" -n "$ns" >/dev/null 2>&1; then
      if ! kubectl get pvc "$pv2_pvc" -n "$ns" -o json \
          | jq -e --arg k "$CREATED_BY_LABEL_KEY" --arg v "$MIGRATION_TOOL_ID" '.metadata.labels[$k]==$v' >/dev/null; then
        conflicts+=("PVC/$ns/$pv2_pvc (exists without ${CREATED_BY_LABEL_KEY}=${MIGRATION_TOOL_ID})")
      fi
    fi

    # Need the bound PV to derive intermediate PV/PVC + snapshot names
    local pv
    pv=$(kubectl get pvc "$pvc" -n "$ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
    [[ -z "$pv" ]] && continue

    # Determine if in-tree azureDisk (creates intermediate resources)
    local diskuri
    diskuri=$(kubectl get pv "$pv" -o jsonpath='{.spec.azureDisk.diskURI}' 2>/dev/null || true)

    if [[ -n "$diskuri" ]]; then
      # Intermediate PVC
      local int_pvc
      int_pvc="$(name_csi_pvc "$pvc")"
      if kubectl get pvc "$int_pvc" -n "$ns" >/dev/null 2>&1; then
        if ! kubectl get pvc "$int_pvc" -n "$ns" -o json \
            | jq -e --arg k "$CREATED_BY_LABEL_KEY" --arg v "$MIGRATION_TOOL_ID" '.metadata.labels[$k]==$v' >/dev/null; then
          conflicts+=("PVC/$ns/$int_pvc (intermediate) exists without ${CREATED_BY_LABEL_KEY}=${MIGRATION_TOOL_ID}")
        fi
      fi
      # Intermediate PV
      local int_pv
      int_pv="$(name_csi_pv "$pv")"
      if kubectl get pv "$int_pv" >/dev/null 2>&1; then
        if ! kubectl get pv "$int_pv" -o json \
            | jq -e --arg k "$CREATED_BY_LABEL_KEY" --arg v "$MIGRATION_TOOL_ID" '.metadata.labels[$k]==$v' >/dev/null; then
          conflicts+=("PV/$int_pv (intermediate) exists without ${CREATED_BY_LABEL_KEY}=${MIGRATION_TOOL_ID}")
        fi
      fi
    fi

    # Snapshot (regardless of in-tree vs csi: name derived from PV)
    local snap
    snap="$(name_snapshot "$pv")"
    if kubectl get volumesnapshot "$snap" -n "$ns" >/dev/null 2>&1; then
      if ! kubectl get volumesnapshot "$snap" -n "$ns" -o json \
          | jq -e --arg k "$CREATED_BY_LABEL_KEY" --arg v "$MIGRATION_TOOL_ID" '.metadata.labels[$k]==$v' >/dev/null; then
        conflicts+=("VolumeSnapshot/$ns/$snap (exists without ${CREATED_BY_LABEL_KEY}=${MIGRATION_TOOL_ID})")
      fi
    fi
  done

  if (( ${#conflicts[@]} > 0 )); then
    if [[ "${1:-}" == "collect" ]]; then
      # Populate global array for unified reporting
      CONFLICT_ISSUES=("${conflicts[@]}")
      return 0
    fi
    err "Name conflict preflight failed: found ${#conflicts[@]} pre-existing object(s) that could be overwritten."
    printf 'Conflicts:\n'
    printf '  - %s\n' "${conflicts[@]}"
    echo
    err "Resolve (delete or rename) these objects OR label them with ${CREATED_BY_LABEL_KEY}=${MIGRATION_TOOL_ID} if they are safe (not recommended) before re-running."
    exit 1
  fi

  ok "No conflicting pre-existing objects detected."
}

check_premium_lrs() {
  local pvc="$1" ns="$2"
  local sc sku sat
  sc=$(kget pvc "$pvc" -n "$ns" -o jsonpath='{.spec.storageClassName}' 2>/dev/null || true)
  [[ -z "$sc" ]] && return 1
  sku=$(kget sc "$sc" -o jsonpath='{.parameters.skuName}' 2>/dev/null || true)
  sat=$(kget sc "$sc" -o jsonpath='{.parameters.storageaccounttype}' 2>/dev/null || true)
  [[ -z "$sku" && -z "$sat" ]] && return 1
  if { [[ -z "$sku" || "$sku" == "Premium_LRS" ]] && [[ -z "$sat" || "$sat" == "Premium_LRS" ]]; }; then
    return 0
  fi
  return 1
}

ensure_snapshot_class() {
  if kubectl_retry get volumesnapshotclass "$SNAPSHOT_CLASS" >/dev/null 2>&1; then
    info "VolumeSnapshotClass '$SNAPSHOT_CLASS' already exists"
    return
  fi
  info "Creating VolumeSnapshotClass '$SNAPSHOT_CLASS'"
  if ! kapply_retry <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: $SNAPSHOT_CLASS
  labels:
    ${CREATED_BY_LABEL_KEY}: ${MIGRATION_TOOL_ID}
driver: disk.csi.azure.com
deletionPolicy: Delete
parameters:
  incremental: "true"
EOF
  then 
    audit_add "VolumeSnapshotClass" "$SNAPSHOT_CLASS" "" "create-failed" "N/A" "forSnapshots=true reason=applyFailure"
    return 1
  else
    audit_add "VolumeSnapshotClass" "$SNAPSHOT_CLASS" "" "create" "kubectl delete volumesnapshotclass $SNAPSHOT_CLASS" "forSnapshots=true"
  fi
}

apply_storage_class_variant() {
  local base="$1" suffix="$2" sku="$3"
  local sc_name="${base}-${suffix}"
  if kubectl_retry get sc "$sc_name" >/dev/null 2>&1; then
    info "StorageClass $sc_name exists"
    return
  fi
  local params_json params_filtered
  params_json=$(kubectl_retry get sc "$base" -o json 2>/dev/null || true)
  [[ -z "$params_json" ]] && { warn "Cannot fetch base SC $base; skipping variant"; return; }
  params_filtered=$(echo "$params_json" | jq -r '
      .parameters
      | to_entries
      | map(select(.key != "skuName"
                   and .key != "storageaccounttype"
                   and .key != "cachingMode"
                   and (.key | test("encryption";"i") | not)))
      | map("  " + .key + ": \"" + (.value|tostring) + "\"")
      | join("\n")
    ')
  info "Creating StorageClass $sc_name (skuName=$sku)"
  if ! kapply_retry <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $sc_name
  labels:
    ${CREATED_BY_LABEL_KEY}: ${MIGRATION_TOOL_ID}
provisioner: disk.csi.azure.com
parameters:
  skuName: $sku
  cachingMode: None
$params_filtered
reclaimPolicy: Retain
allowVolumeExpansion: true
volumeBindingMode: Immediate
EOF
  then
    audit_add "StorageClass" "$sc_name" "" "create-failed" "N/A" "variantOf=${base} sku=${sku} reason=applyFailure"
    return 1
  else
      audit_add "StorageClass" "$sc_name" "" "create" "kubectl delete sc $sc_name" "variantOf=${base} sku=${sku}"
  fi
}

create_variants_for_sources() {
  local sources=("$@")
  for sc in "${sources[@]}"; do
    local sku sat
    sku=$(kubectl get sc "$sc" -o jsonpath='{.parameters.skuName}' 2>/dev/null || true)
    sat=$(kubectl get sc "$sc" -o jsonpath='{.parameters.storageaccounttype}' 2>/dev/null || true)
    if [[ -n "$sku" || -n "$sat" ]]; then
      if ! { [[ -z "$sku" || "$sku" == "Premium_LRS" ]] && [[ -z "$sat" || "$sat" == "Premium_LRS" ]]; }; then
        warn "Source SC $sc not Premium_LRS (skuName=$sku storageaccounttype=$sat) – skipping variants"
        continue
      fi
    fi
    apply_storage_class_variant "$sc" "pv2" "PremiumV2_LRS"
    apply_storage_class_variant "$sc" "pv1" "Premium_LRS"
  done
}

# NEW: show pods still using the PVC
list_pods_using_pvc() {
  local ns="$1" pvc="$2"
  kubectl get pods -n "$ns" \
    --field-selector spec.volumes.persistentVolumeClaim.claimName="$pvc" \
    -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,NODE:.spec.nodeName' 2>/dev/null || true
}

# NEW: wait for workload detach if VolumeAttachment(s) present
wait_for_workload_detach() {
  local pv="$1" pvc="$2" ns="$3"
  local timeout_deadline=0
  if (( WORKLOAD_DETACH_TIMEOUT_MINUTES > 0 )); then
    timeout_deadline=$(( $(date +%s) + WORKLOAD_DETACH_TIMEOUT_MINUTES * 60 ))
  fi
  info "Waiting for workload to stop using PV $pv (PVC $ns/$pvc)."
  while true; do
    # collect attachments
    local attachments
    attachments=$(kubectl get volumeattachment -o jsonpath="{range .items[?(@.spec.source.persistentVolumeName=='$pv')]}{.metadata.name}{'\n'}{end}" 2>/dev/null || true)
    if [[ -z "$attachments" ]]; then
      ok "No VolumeAttachment found for $pv; proceeding."
      return 0
    fi
    echo
    warn "Still attached (VolumeAttachment present) for PV=$pv:"
    echo "$attachments" | sed 's/^/  - /'
    echo "Pods referencing PVC (namespace $ns):"
    list_pods_using_pvc "$ns" "$pvc" | sed 's/^/  /' || true
    echo
    info "Action required: scale down or delete workloads using PVC $ns/$pvc. Retrying in ${POLL_INTERVAL}s..."
    if (( timeout_deadline > 0 )) && (( $(date +%s) >= timeout_deadline )); then
      warn "Workload detach wait timeout (${WORKLOAD_DETACH_TIMEOUT_MINUTES}m) reached for $ns/$pvc; skipping this PVC."
      return 1
    fi
    sleep "$POLL_INTERVAL"
  done
}

create_csi_pv_pvc() {
  local pvc="$1" ns="$2" pv="$3" size="$4" mode="$5" sc="$6" diskURI="$7"
  local csi_pv="$(name_csi_pv "$pv")"
  local csi_pvc="$(name_csi_pvc "$pvc")"

  if kubectl_retry get pvc "$csi_pvc" -n "$ns" >/dev/null 2>&1; then
    if kubectl_retry get pvc "$csi_pvc" -n "$ns" -o json | jq -e --arg key "$CREATED_BY_LABEL_KEY" --arg tool "$MIGRATION_TOOL_ID" '.metadata.labels[$key] == $tool' >/dev/null; then
      info "Intermediate PVC $ns/$csi_pvc already exists"
      return
    fi
    warn "Intermediate PVC $ns/$csi_pvc exists but label missing"
    return
  fi

  info "Creating intermediate PV/PVC $csi_pv / $csi_pvc"
  if ! kapply_retry <<EOF
apiVersion: v1
kind: PersistentVolume
metadata:
  name: $csi_pv
  labels:
    ${CREATED_BY_LABEL_KEY}: ${MIGRATION_TOOL_ID}
  annotations:
    pv.kubernetes.io/provisioned-by: disk.csi.azure.com
spec:
  capacity:
    storage: $size
  accessModes:
  - ReadWriteOnce
  claimRef:
    apiVersion: v1
    kind: PersistentVolumeClaim
    name: $csi_pvc
    namespace: $ns
  csi:
    driver: disk.csi.azure.com
    volumeHandle: $diskURI
    volumeAttributes:
      csi.storage.k8s.io/pv/name: $csi_pv
      csi.storage.k8s.io/pvc/name: $csi_pvc
      csi.storage.k8s.io/pvc/namespace: $ns
      requestedsizegib: "$size"
      skuname: Premium_LRS
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ${sc}-pv1
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $csi_pvc
  namespace: $ns
  labels:
    disk.csi.azure.com/pvc: "true"
    ${CREATED_BY_LABEL_KEY}: ${MIGRATION_TOOL_ID}
spec:
  accessModes:
  - ReadWriteOnce
  volumeMode: ${mode}
  resources:
    requests:
      storage: $size
  storageClassName: ${sc}-pv1
  volumeName: $csi_pv
EOF
  then
    audit_add "PersistentVolume" "$csi_pv" "" "create-failed" "N/A" "intermediate=true sourceDiskURI=$diskURI reason=applyFailure"
    audit_add "PersistentVolumeClaim" "$csi_pvc" "$ns" "create-failed" "N/A" "intermediate=true sc=${sc}-pv1 reason=applyFailure"
    return 1
  else
    audit_add "PersistentVolume" "$csi_pv" "" "create" "kubectl delete pv $csi_pv" "intermediate=true sourceDiskURI=$diskURI"
    audit_add "PersistentVolumeClaim" "$csi_pvc" "$ns" "create" "kubectl delete pvc $csi_pvc -n $ns" "intermediate=true sc=${sc}-pv1"
  fi

  if ! kubectl_retry wait pvc "${csi_pvc}" -n "$ns" --for=jsonpath='{.status.phase}=Bound' --timeout=120s >/dev/null 2>&1; then
    warn "Intermediate PVC $ns/$csi_pvc not bound in time"
    return
  fi
}

ensure_snapshot() {
  # Args:
  #   $1 = source PVC name to snapshot (may be original PVC or intermediate CSI PVC)
  #   $2 = namespace
  #   $3 = PV name (only to derive stable snapshot name)
  local source_pvc="$1" ns="$2" pv="$3"
  local snapshot
  snapshot="$(name_snapshot "$pv")"

  local snapshot_exists=false
  if kubectl_retry get volumesnapshot "$snapshot" -n "$ns" >/dev/null 2>&1; then
    snapshot_exists=true
  fi

  local need_create=false
  local recreated=false

  if [[ "$snapshot_exists" == "true" ]]; then
    # Evaluate staleness of existing snapshot
    local ready prev_rv current_rv creation_ts creation_epoch now_epoch age
    local stale=false
    local reasons=()
    ready=$(kubectl get volumesnapshot "$snapshot" -n "$ns" -o jsonpath='{.status.readyToUse}' 2>/dev/null || true)
    prev_rv=$(kubectl get volumesnapshot "$snapshot" -n "$ns" -o jsonpath='{.metadata.annotations.disk\.csi\.azure\.com/source-pvc-rv}' 2>/dev/null || true)
    creation_ts=$(kubectl get volumesnapshot "$snapshot" -n "$ns" -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || true)
    current_rv=$(kubectl get pvc "$source_pvc" -n "$ns" -o jsonpath='{.metadata.resourceVersion}' 2>/dev/null || true)

    if [[ -n "$creation_ts" ]]; then
      creation_epoch=$(date -d "$creation_ts" +%s 2>/dev/null || echo 0)
      now_epoch=$(date +%s)
      if (( SNAPSHOT_MAX_AGE_SECONDS > 0 )) && (( now_epoch - creation_epoch > SNAPSHOT_MAX_AGE_SECONDS )); then
        stale=true
        reasons+=("age>$SNAPSHOT_MAX_AGE_SECONDS")
        age=$(( now_epoch - creation_epoch ))
      fi
    fi
    if [[ -n "$prev_rv" && -n "$current_rv" && "$prev_rv" != "$current_rv" ]]; then
      stale=true
      reasons+=("sourcePVCResourceVersionChanged($prev_rv->$current_rv)")
    fi

    if [[ "$stale" == "true" ]]; then
      if [[ "$SNAPSHOT_RECREATE_ON_STALE" == "true" ]]; then
        warn "Snapshot $ns/$snapshot stale (${reasons[*]}). Deleting for recreation."
        kubectl_retry delete volumesnapshot "$snapshot" -n "$ns" --ignore-not-found
        audit_add "VolumeSnapshot" "$snapshot" "$ns" "delete" "N/A" "reason=stale (${reasons[*]})"
        # Wait briefly for deletion to finalize
        for _ in {1..12}; do
          if ! kubectl get volumesnapshot "$snapshot" -n "$ns" >/dev/null 2>&1; then
            snapshot_exists=false
            need_create=true
            recreated=true
            break
          fi
          sleep 5
        done
        if [[ "$snapshot_exists" == "true" ]]; then
          warn "Timed out deleting stale snapshot $ns/$snapshot; will proceed WITHOUT recreation (treating existing snapshot as-is)."
        fi
      else
        warn "Snapshot $ns/$snapshot stale (${reasons[*]}) but SNAPSHOT_RECREATE_ON_STALE=false (reusing)."
      fi
    fi

    # Fast path: reuse existing ready snapshot (and not recreating)
    if [[ "$snapshot_exists" == "true" && "$need_create" == "false" ]]; then
      local r_ready
      r_ready=$(kubectl get volumesnapshot "$snapshot" -n "$ns" -o jsonpath='{.status.readyToUse}' 2>/dev/null || true)
      if [[ "$r_ready" == "true" ]]; then
        info "Snapshot $ns/$snapshot ready (reused)"
        return 0
      fi
      info "Waiting on existing snapshot $ns/$snapshot to become ready"
    fi
  else
    need_create=true
  fi

  if [[ "$need_create" == "true" ]]; then
    info "Creating snapshot $ns/$snapshot (source PVC: $source_pvc)"
    local source_rv
    source_rv=$(kubectl get pvc "$source_pvc" -n "$ns" -o jsonpath='{.metadata.resourceVersion}' 2>/dev/null || true)
    if ! kapply_retry <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: $snapshot
  namespace: $ns
  labels:
    ${CREATED_BY_LABEL_KEY}: ${MIGRATION_TOOL_ID}
  annotations:
    disk.csi.azure.com/source-pvc: "$source_pvc"
    disk.csi.azure.com/source-pvc-rv: "$source_rv"
spec:
  volumeSnapshotClassName: $SNAPSHOT_CLASS
  source:
    persistentVolumeClaimName: $source_pvc
EOF
    then
      audit_add "VolumeSnapshot" "$snapshot" "$ns" "create-failed" "N/A" "sourcePVC=$source_pvc reason=applyFailure recreate=$recreated"
      return 1
    else
      local extra="sourcePVC=$source_pvc sourceRV=$source_rv"
      if [[ "$recreated" == "true" ]]; then
        extra="$extra recreate=true"
      fi
      audit_add "VolumeSnapshot" "$snapshot" "$ns" "create" "kubectl delete volumesnapshot $snapshot -n $ns" "$extra"
    fi
  fi

  # Wait for readiness (covers both fresh creation & not-ready reuse cases)
  for _ in {1..36}; do
    local r
    r=$(kubectl_retry get volumesnapshot "$snapshot" -n "$ns" -o jsonpath='{.status.readyToUse}' 2>/dev/null || true)
    if [[ "$r" == "true" ]]; then
      ok "Snapshot $ns/$snapshot ready"
      return 0
    fi
    sleep 5
  done

  warn "Snapshot $ns/$snapshot not ready after timeout"
  return 1
}

ensure_pv2_pvc() {
  local pvc="$1" ns="$2" pv="$3" size="$4" mode="$5" sc="$6"
  local pv2_pvc="$(name_pv2_pvc "$pvc")"
  local snapshot="$(name_snapshot "$pv")"

  if kubectl_retry get pvc "$pv2_pvc" -n "$ns" >/dev/null 2>&1; then
    info "PV2 PVC $ns/$pv2_pvc exists"
    return
  fi

  info "Creating PV2 PVC $ns/$pv2_pvc"
  if ! kapply_retry <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pv2_pvc
  namespace: $ns
  labels:
    ${CREATED_BY_LABEL_KEY}: ${MIGRATION_TOOL_ID}
spec:
  accessModes:
  - ReadWriteOnce
  volumeMode: $mode
  storageClassName: ${sc}-pv2
  resources:
    requests:
      storage: $size
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: $snapshot
EOF
  then
    audit_add "PersistentVolumeClaim" "$pv2_pvc" "$ns" "create-failed" "N/A" "pv2=true sc=${sc}-pv2 snapshot=${snapshot} reason=applyFailure"
    return 1
  else
    audit_add "PersistentVolumeClaim" "$pv2_pvc" "$ns" "create" "kubectl delete pvc $pv2_pvc -n $ns" "pv2=true sc=${sc}-pv2 snapshot=${snapshot}"
  fi
}

extract_event_reason() {
  local ns="$1" pv2_pvc="$2"
  kubectl get events -n "$ns" -o json 2>/dev/null \
    | jq -r --arg name "$pv2_pvc" '
        .items
        | map(select(.involvedObject.name==$name
                     and (.reason|test("SKUMigration(Completed|Progress|Started)"))))
        | sort_by(.lastTimestamp)
        | last?.reason // empty' 2>/dev/null || true
}

# ---------------- Discover Tagged PVCs ----------------
declare -a MIG_PVCS
if [[ -z "$NAMESPACE" ]]; then
  mapfile -t MIG_PVCS < <(kubectl get pvc --all-namespaces -l "$MIGRATION_LABEL" \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"|"}{.metadata.name}{"\n"}{end}')
else
  mapfile -t MIG_PVCS < <(kubectl get pvc -n "$NAMESPACE" -l "$MIGRATION_LABEL" \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"|"}{.metadata.name}{"\n"}{end}')
fi

total_found=${#MIG_PVCS[@]}
if (( total_found == 0 )); then
  warn "No PVCs found with label $MIGRATION_LABEL"
  exit 0
fi
IFS=$'\n' MIG_PVCS=($(printf '%s\n' "${MIG_PVCS[@]}" | sort)) # sort for idempotent order
if (( total_found > MAX_PVCS )); then
  warn "Found $total_found PVCs; processing only first $MAX_PVCS (adjust MAX_PVCS to change)"
  MIG_PVCS=("${MIG_PVCS[@]:0:MAX_PVCS}")
fi

info "Processing PVCs:"
printf '  %s\n' "${MIG_PVCS[@]}"

# ---------------- New: Prerequisites & Conflict Aggregation ----------------
declare -a PREREQ_ISSUES
declare -a CONFLICT_ISSUES

run_prerequisites_checks() {
  info "Running migration pre-requisites validation (non-mutating)..."
  local max_selected=${#MIG_PVCS[@]}
  if (( max_selected > 50 )); then
    PREREQ_ISSUES+=("Selected PVC set size ($max_selected) exceeds recommended batch limit of 50")
  fi

  # Cache SC json to avoid repeated kubectl calls
  declare -A _SC_JSON_CACHE

  for ENTRY in "${MIG_PVCS[@]}"; do
    local ns="${ENTRY%%|*}"
    local pvc="${ENTRY##*|}"

    # Bound?
    local pv
    pv=$(kubectl get pvc "$pvc" -n "$ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
    if [[ -z "$pv" ]]; then
      PREREQ_ISSUES+=("PVC/$ns/$pvc not bound (no PV name yet)")
      continue
    fi

    # SC
    local sc
    sc=$(kubectl get pvc "$pvc" -n "$ns" -o jsonpath='{.spec.storageClassName}' 2>/dev/null || true)
    if [[ -z "$sc" ]]; then
      PREREQ_ISSUES+=("PVC/$ns/$pvc has no storageClassName (cannot evaluate parameters)")
    fi

    # PV capacity
    local size
    size=$(kubectl get pv "$pv" -o jsonpath='{.spec.capacity.storage}' 2>/dev/null || true)
    if [[ -z "$size" ]]; then
      PREREQ_ISSUES+=("PV/$pv capacity missing")
    fi

    # Attempt zone detection
    local zone
    zone=$(kubectl get pv "$pv" -o jsonpath='{.spec.nodeAffinity.required.nodeSelectorTerms[*].matchExpressions[?(@.key=="topology.disk.csi.azure.com/zone")].values[0]}' 2>/dev/null || true)
    if [[ -z "$zone" ]]; then
      zone=$(kubectl get pv "$pv" -o jsonpath='{.spec.nodeAffinity.required.nodeSelectorTerms[*].matchExpressions[?(@.key=="topology.kubernetes.io/zone")].values[0]}' 2>/dev/null || true)
    fi
    if [[ -z "$zone" ]]; then
      zone=$(kubectl get pv "$pv" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}' 2>/dev/null || true)
    fi
    if [[ -z "$zone" ]]; then
      PREREQ_ISSUES+=("PV/$pv has no detectable zone topology (migration requires zonal disk)")
    fi

    # Inspect StorageClass parameters (if available)
    local sc_json=""
    if [[ -n "$sc" ]]; then
      if [[ -z "${_SC_JSON_CACHE[$sc]:-}" ]]; then
        _SC_JSON_CACHE[$sc]=$(kubectl get sc "$sc" -o json 2>/dev/null || echo "")
      fi
      sc_json="${_SC_JSON_CACHE[$sc]}"
    fi
    if [[ -n "$sc_json" ]]; then
      local cachingMode enableBursting diskEncryptionType logicalSector perfProfile
      cachingMode=$(echo "$sc_json" | jq -r '.parameters.cachingMode // empty' 2>/dev/null || true)
      enableBursting=$(echo "$sc_json" | jq -r '.parameters.enableBursting // empty' 2>/dev/null || true)
      diskEncryptionType=$(echo "$sc_json" | jq -r '.parameters.diskEncryptionType // empty' 2>/dev/null || true)
      logicalSector=$(echo "$sc_json" | jq -r '.parameters.LogicalSectorSize // empty' 2>/dev/null || true)
      perfProfile=$(echo "$sc_json" | jq -r '.parameters.perfProfile // empty' 2>/dev/null || true)

      # cachingMode must be unset or None
      if [[ -n "$cachingMode" ]] && ! [[ "${cachingMode,,}" == "none" ]]; then
        PREREQ_ISSUES+=("StorageClass/$sc (PVC $ns/$pvc) cachingMode='$cachingMode' (must be none or unset)")
      fi
      # enableBursting must be absent or false
      if [[ -n "$enableBursting" ]] && ! [[ "${enableBursting,,}" == "false" ]]; then
        PREREQ_ISSUES+=("StorageClass/$sc (PVC $ns/$pvc) enableBursting='$enableBursting' (must be false or unset)")
      fi
      # diskEncryptionType must not be double encryption
      if [[ "$diskEncryptionType" == "EncryptionAtRestWithPlatformAndCustomerKeys" ]]; then
        PREREQ_ISSUES+=("StorageClass/$sc (PVC $ns/$pvc) uses double encryption (unsupported)")
      fi
      # LogicalSectorSize must be 512 (if set)
      if [[ -n "$logicalSector" ]] && [[ "$logicalSector" != "512" ]]; then
        PREREQ_ISSUES+=("StorageClass/$sc (PVC $ns/$pvc) LogicalSectorSize=$logicalSector (expected 512)")
      fi
      # perfProfile if set must be 'basic' (treat others as issue)
      if [[ -n "$perfProfile" ]] && [[ "${perfProfile,,}" != "basic" ]]; then
        PREREQ_ISSUES+=("StorageClass/$sc (PVC $ns/$pvc) perfProfile='$perfProfile' (must be basic or unset)")
      fi
    else
      PREREQ_ISSUES+=("StorageClass/$sc (referenced by PVC $ns/$pvc) not retrievable")
    fi

    # Detect double encryption alternative naming (some docs use encryptionAtRestWithPlatformAndCustomerKeys)
    # Already covered above; no duplicate check.

    # Determine provisioning mode (for info)
    local intree
    intree=$(kubectl get pv "$pv" -o jsonpath='{.spec.azureDisk.diskURI}' 2>/dev/null || true)
    # If neither azureDisk nor CSI driver recognized => flag
    if [[ -z "$intree" ]]; then
      local drv
      drv=$(kubectl get pv "$pv" -o jsonpath='{.spec.csi.driver}' 2>/dev/null || true)
      if [[ "$drv" != "disk.csi.azure.com" ]]; then
        PREREQ_ISSUES+=("PV/$pv provisioning type unknown (neither in-tree azureDisk nor CSI disk.csi.azure.com)")
      fi
    fi
  done

  # Informational (not blocking) – cannot auto-validate subscription PV2 quota
  info "NOTE: Subscription/regional PremiumV2 quota (default 1000) not automatically checked."
}

print_combined_validation_report_and_exit_if_needed() {
  local prereq_count=${#PREREQ_ISSUES[@]}
  local conflict_count=${#CONFLICT_ISSUES[@]}
  if (( prereq_count == 0 && conflict_count == 0 )); then
    ok "All migration pre-requisites and name conflict checks passed."
    return 0
  fi
  echo
  err "Pre-run validation failed:"
  if (( prereq_count > 0 )); then
    echo "  Pre-requisite issues (${prereq_count}):"
    printf '    - %s\n' "${PREREQ_ISSUES[@]}"
  fi
  if (( conflict_count > 0 )); then
    echo "  Naming conflicts (${conflict_count}):"
    printf '    - %s\n' "${CONFLICT_ISSUES[@]}"
  fi
  echo
  err "Resolve the above and re-run. (No resources were mutated.)"
  exit 1
}

run_prerequisites_and_conflicts() {
  run_prerequisites_checks
  # Collect conflicts without exiting
  ensure_no_foreign_conflicts collect
  print_combined_validation_report_and_exit_if_needed
}

# Invoke new unified preflight validator (replaces direct ensure_no_foreign_conflicts call)
run_prerequisites_and_conflicts

# ---------------- Collect Unique Source Source SCs ----------------
declare -A SC_SET
for entry in "${MIG_PVCS[@]}"; do
  ns="${entry%%|*}"
  pvc="${entry##*|}"
  sc=$(kubectl get pvc "$pvc" -n "$ns" -o jsonpath='{.spec.storageClassName}' 2>/dev/null || true)
  [[ -n "$sc" ]] && SC_SET["$sc"]=1
done
SOURCE_SCS=("${!SC_SET[@]}")
info "Source StorageClasses: ${SOURCE_SCS[*]}"

ensure_snapshot_class
create_variants_for_sources "${SOURCE_SCS[@]}"

# ---------------- Main Migration Loop ----------------
for ENTRY in "${MIG_PVCS[@]}"; do
  pvc_ns="${ENTRY%%|*}"
  pvc="${ENTRY##*|}"

  if ! check_premium_lrs "$pvc" "$pvc_ns"; then
    info "PVC $pvc (ns=$pvc_ns) not Premium_LRS. Skipping."
    continue
  fi

  DONE_LABEL=$(kubectl get pvc "$pvc" -n "$pvc_ns" \
    -o jsonpath="{.metadata.labels['$MIGRATION_DONE_LABEL_KEY']}" 2>/dev/null || true)
  if [[ "$DONE_LABEL" == "$MIGRATION_DONE_LABEL_VALUE" ]]; then
    info "Migration already done for $pvc (ns=$pvc_ns). Skipping."
    continue
  fi

  pv=$(kubectl get pvc "$pvc" -n "$pvc_ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
  if [[ -z "$pv" ]]; then
    warn "PVC $pvc_ns/$pvc not yet bound (no PV); skipping for now"
    continue
  fi

  # CHANGED: instead of skipping if workload active, wait (optional)
  attachments=$(kubectl get volumeattachment -o jsonpath="{range .items[?(@.spec.source.persistentVolumeName=='$pv')]}{.metadata.name}{'\n'}{end}" 2>/dev/null || true)
  if [[ -n "$attachments" ]]; then
    if [[ "$WAIT_FOR_WORKLOAD" == "true" ]]; then
      wait_for_workload_detach "$pv" "$pvc" "$pvc_ns" || { warn "Proceeding without migrating $pvc_ns/$pvc due to workload still attached"; continue; }
    else
      warn "Workload still attached (VolumeAttachment present) for $pv; skipping (WAIT_FOR_WORKLOAD=false)"
      continue
    fi
  else
    info "No VolumeAttachment found for volume $pv. Workload is not running."
  fi

  RECLAIMPOLICY=$(kubectl get pv "$pv" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}' 2>/dev/null || true)
  info "Reclaim Policy for Persistent Volume $pv is $RECLAIMPOLICY"
  if [[ "$RECLAIMPOLICY" == "Delete" ]]; then
    info "Updating ReclaimPolicy for $pv to Retain"
    if kubectl_retry patch pv "$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}' >/dev/null 2>&1; then
      audit_add "PersistentVolume" "$pv" "" "patch" \
        "kubectl patch pv $pv -p '{\"spec\":{\"persistentVolumeReclaimPolicy\":\"Delete\"}}'" \
        "reclaimPolicy Delete->Retain"
    fi
  fi

  mode=$(kubectl get pv "$pv" -o jsonpath='{.spec.volumeMode}' 2>/dev/null || true)
  sc=$(kubectl get pv "$pv" -o jsonpath='{.spec.storageClassName}' 2>/dev/null || true)
  size=$(kubectl get pv "$pv" -o jsonpath='{.spec.capacity.storage}' 2>/dev/null || true)
  diskuri=$(kubectl get pv "$pv" -o jsonpath='{.spec.azureDisk.diskURI}' 2>/dev/null || true)
  info "volumeMode: $mode , storage_class: $sc , storage: $size , diskURI: $diskuri"

  # Determine provisioning type:
  # In-tree azureDisk => .spec.azureDisk.diskURI present
  # CSI azure disk    => .spec.csi.driver == disk.csi.azure.com
  csi_driver=$(kubectl get pv "$pv" -o jsonpath='{.spec.csi.driver}' 2>/dev/null || true)

  if [[ -n "$diskuri" ]]; then
    # In-tree path: need sc, size, diskuri, and both pv1/pv2 SC variants.
    if [[ -z "$sc" || -z "$size" ]]; then
      warn "Missing required PV info for in-tree volume (sc=$sc size=$size); skipping"
      continue
    fi
    if ! kubectl get sc "${sc}-pv1" >/dev/null 2>&1; then
      warn "Missing ${sc}-pv1 StorageClass (required for intermediate PV/PVC); skipping"
      continue
    fi
    if ! kubectl get sc "${sc}-pv2" >/dev/null 2>&1; then
      warn "Missing ${sc}-pv2 StorageClass; skipping"
      continue
    fi
    # Create intermediate CSI PV/PVC (idempotent) then snapshot that.
    create_csi_pv_pvc "$pvc" "$pvc_ns" "$pv" "$size" "$mode" "$sc" "$diskuri"
    snapshot_source_pvc="$(name_csi_pvc "$pvc")"
  else
    # CSI path: expect driver to be disk.csi.azure.com; we do NOT create intermediate PV/PVC.
    if [[ "$csi_driver" != "disk.csi.azure.com" ]]; then
      warn "PV $pv is neither in-tree azureDisk nor CSI disk.csi.azure.com (driver='$csi_driver'); skipping"
      continue
    fi
    if [[ -z "$sc" || -z "$size" ]]; then
      warn "Missing required PV info for CSI volume (sc=$sc size=$size); skipping"
      continue
    fi
    if ! kubectl get sc "${sc}-pv2" >/dev/null 2>&1; then
      warn "Missing ${sc}-pv2 StorageClass; skipping"
      continue
    fi
    snapshot_source_pvc="$pvc"
  fi

  if ! ensure_snapshot "$snapshot_source_pvc" "$pvc_ns" "$pv"; then
    warn "Snapshot failed for $pvc_ns/$pvc; skipping PVC."
    continue
  fi

  ensure_pv2_pvc "$pvc" "$pvc_ns" "$pv" "$size" "$mode" "$sc"
done

# ---------------- Monitoring Loop ----------------
deadline=$(( $(date +%s) + MONITOR_TIMEOUT_MINUTES*60 ))
info "Monitoring migrations (timeout ${MONITOR_TIMEOUT_MINUTES}m)..."

while true; do
  ALL_DONE=true
  for ENTRY in "${MIG_PVCS[@]}"; do
    pvc_ns="${ENTRY%%|*}"
    pvc="${ENTRY##*|}"
    pv2_pvc="$(name_pv2_pvc "$pvc")"

    if kubectl get pvc "$pvc" -n "$pvc_ns" \
        -o jsonpath="{.metadata.labels['$MIGRATION_DONE_LABEL_KEY']}" 2>/dev/null | grep -q "^${MIGRATION_DONE_LABEL_VALUE}\$"; then
      continue
    fi

    STATUS=$(kubectl get pvc "$pv2_pvc" -n "$pvc_ns" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [[ "$STATUS" != "Bound" ]]; then
      ALL_DONE=false
    fi

    reason=$(extract_event_reason "$pvc_ns" "$pv2_pvc")
    case "$reason" in
      SKUMigrationCompleted)
        kubectl_retry label pvc "$pvc" -n "$pvc_ns" "${MIGRATION_DONE_LABEL_KEY}=${MIGRATION_DONE_LABEL_VALUE}" --overwrite
        ok "Completed $pvc_ns/$pvc"
        # Capture previous label value (may be empty)
        prev_lbl=$(kubectl get pvc "$pvc" -n "$pvc_ns" -o jsonpath="{.metadata.labels['$MIGRATION_DONE_LABEL_KEY']}" 2>/dev/null || true)
        audit_add "PersistentVolumeClaim" "$pvc" "$pvc_ns" "label" \
          "kubectl label pvc '$pvc' -n '$pvc_ns' ${MIGRATION_DONE_LABEL_KEY}-" \
          "set ${MIGRATION_DONE_LABEL_KEY}=${MIGRATION_DONE_LABEL_VALUE}"
         ;;
      SKUMigrationProgress|SKUMigrationStarted)
        info "$reason $pvc_ns/$pv2_pvc"
        ALL_DONE=false
        ;;
      "")
        info "No migration events yet for $pvc_ns/$pv2_pvc"
        ALL_DONE=false
        ;;
    esac

    # BEGIN NEW: Proactive fallback label if no events and threshold exceeded
    if [[ "$STATUS" == "Bound" ]] && [[ -z "$reason" ]] && (( MIGRATION_FORCE_INPROGRESS_AFTER_MINUTES > 0 )); then
      # Original PV name (source)
      orig_pv=$(kubectl get pvc "$pvc" -n "$pvc_ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
      if [[ -n "$orig_pv" ]]; then
        inprog_val=$(kubectl get pv "$orig_pv" -o jsonpath="{.metadata.labels['$MIGRATION_IN_PROGRESS_LABEL_KEY']}" 2>/dev/null || true)
        if [[ "$inprog_val" != "$MIGRATION_IN_PROGRESS_LABEL_VALUE" ]]; then
          # Age of pv2 pvc
            cts=$(kubectl get pvc "$pv2_pvc" -n "$pvc_ns" -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || true)
            if [[ -n "$cts" ]]; then
              creation_epoch=$(date -d "$cts" +%s 2>/dev/null || echo 0)
              now_epoch=$(date +%s)
              if (( now_epoch - creation_epoch >= MIGRATION_FORCE_INPROGRESS_AFTER_MINUTES * 60 )); then
                warn "Forcing ${MIGRATION_IN_PROGRESS_LABEL_KEY}=${MIGRATION_IN_PROGRESS_LABEL_VALUE} on PV $orig_pv (no migration events after ${MIGRATION_FORCE_INPROGRESS_AFTER_MINUTES}m)."
                warn "If events still do not appear, restart Azure Disk CSI controller pods, e.g.: kubectl -n kube-system delete pod -l app=csi-azuredisk-controller"
                kubectl_retry label pv "$orig_pv" "${MIGRATION_IN_PROGRESS_LABEL_KEY}=${MIGRATION_IN_PROGRESS_LABEL_VALUE}" --overwrite
                audit_add "PersistentVolume" "$orig_pv" "" "label" \
                  "kubectl label pv '$orig_pv' ${MIGRATION_IN_PROGRESS_LABEL_KEY}-" \
                  "forced=true reason=noEventsAfter${MIGRATION_FORCE_INPROGRESS_AFTER_MINUTES}m"
              fi
            fi
        fi
      fi
    fi
    # END NEW
  done

  if [[ "$ALL_DONE" == "true" ]]; then
    ok "All processed PVCs either migrated or previously labeled."
    break
  fi
  if (( $(date +%s) >= deadline )); then
    warn "Monitor timeout reached."
    # BEGIN NEW: Final fallback pass on timeout
    info "Final fallback: applying '${MIGRATION_IN_PROGRESS_LABEL_KEY}=${MIGRATION_IN_PROGRESS_LABEL_VALUE}' to source PVs lacking migration events."
    for ENTRY in "${MIG_PVCS[@]}"; do
      pvc_ns="${ENTRY%%|*}"
      pvc="${ENTRY##*|}"
      # Skip already done
      if kubectl get pvc "$pvc" -n "$pvc_ns" \
          -o jsonpath="{.metadata.labels['$MIGRATION_DONE_LABEL_KEY']}" 2>/dev/null | grep -q "^${MIGRATION_DONE_LABEL_VALUE}\$"; then
        continue
      fi
      pv2_pvc="$(name_pv2_pvc "$pvc")"
      if kubectl get pvc "$pv2_pvc" -n "$pvc_ns" >/dev/null 2>&1; then
        orig_pv=$(kubectl get pvc "$pvc" -n "$pvc_ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
        [[ -z "$orig_pv" ]] && continue
        inprog_val=$(kubectl get pv "$orig_pv" -o jsonpath="{.metadata.labels['$MIGRATION_IN_PROGRESS_LABEL_KEY']}" 2>/dev/null || true)
        if [[ "$inprog_val" != "$MIGRATION_IN_PROGRESS_LABEL_VALUE" ]]; then
          warn "Timeout fallback: labeling PV $orig_pv with ${MIGRATION_IN_PROGRESS_LABEL_KEY}=${MIGRATION_IN_PROGRESS_LABEL_VALUE}"
          warn "Restart controller pods if events remain absent: kubectl -n kube-system delete pod -l app=csi-azuredisk-controller"
          kubectl_retry label pv "$orig_pv" "${MIGRATION_IN_PROGRESS_LABEL_KEY}=${MIGRATION_IN_PROGRESS_LABEL_VALUE}" --overwrite
          audit_add "PersistentVolume" "$orig_pv" "" "label" \
            "kubectl label pv '$orig_pv' ${MIGRATION_IN_PROGRESS_LABEL_KEY}-" \
            "forced=true reason=monitorTimeout"
        fi
      fi
    done
    # END NEW
    break
  fi
  sleep "$POLL_INTERVAL"
done

# ---------------- Audit Trail Output ----------------
if [[ "$AUDIT_ENABLE" == "true" ]]; then
  # Compute end-of-run times right before producing the audit summary
  SCRIPT_END_TS="$(date +'%Y-%m-%dT%H:%M:%S')"
  SCRIPT_END_EPOCH="$(date +%s)"
  SCRIPT_DURATION_SEC=$(( SCRIPT_END_EPOCH - SCRIPT_START_EPOCH ))
  SCRIPT_DURATION_FMT="$(human_duration "$SCRIPT_DURATION_SEC")"

  echo
  info "Run Timing:"
  echo "  Start: ${SCRIPT_START_TS}"
  echo "  End:   ${SCRIPT_END_TS}"
  echo "  Elapsed: ${SCRIPT_DURATION_FMT} (${SCRIPT_DURATION_SEC}s)"

  info "Audit Trail (most recent run actions)"
  if (( ${#AUDIT_LINES[@]} == 0 )); then
    echo "  (no mutating actions recorded)"
  else
    echo
    info "Best-effort revert command summary (grouped):"
    declare -A REVERT_COUNT
    for line in "${AUDIT_LINES[@]}"; do
      IFS='|' read -r _ act _ _ _ revert _ <<<"$line"
      [[ "$revert" == "N/A" || -z "$revert" ]] && continue
      REVERT_COUNT["$revert"]=$(( ${REVERT_COUNT["$revert"]:-0} + 1 ))
    done
    for cmd in "${!REVERT_COUNT[@]}"; do
      printf '  (%d) %s\n' "${REVERT_COUNT[$cmd]}" "$cmd"
    done
    unset REVERT_COUNT
  fi

  # Append run footer & full audit lines to log file if configured
  if [[ -n "${AUDIT_LOG_FILE:-}" ]]; then
    {
      # Re-emit timing in the log file for correlation
      echo "RUN_END|${SCRIPT_END_TS}|durationSeconds=${SCRIPT_DURATION_SEC}|durationHuman=${SCRIPT_DURATION_FMT}"
      # Optionally dump the run's audit lines in one block (prefixed for clarity)
      for line in "${AUDIT_LINES[@]}"; do
        echo "AUDIT_LINE|$line"
      done
    } >>"$AUDIT_LOG_FILE" 2>/dev/null || true
  fi
fi

# ---------------- Summary ----------------
echo
info "Summary:"
for entry in "${MIG_PVCS[@]}"; do
  ns="${entry%%|*}"
  pvc="${entry##*|}"
  lbl=$(kubectl get pvc "$pvc" -n "$ns" \
      -o jsonpath="{.metadata.labels['$MIGRATION_DONE_LABEL_KEY']}" 2>/dev/null || true)
  if [[ "$lbl" == "$MIGRATION_DONE_LABEL_VALUE" ]]; then
    echo "  ✓ $ns/$pvc migrated"
  else
    echo "  • $ns/$pvc NOT completed"
  fi
done

ok "Script finished."