#!/usr/bin/env bash
#
# Show a cluster's captured configuration history, newest last.
#
#   ./hack/dev/history.sh              # the default dev cluster
#   ./hack/dev/history.sh pg-source
#   ./hack/dev/history.sh pg-source shared_buffers work_mem
#
# Snapshot keys are content-addressed and prefixed with a UTC timestamp, so the
# lexical order this prints is chronological. The timestamps are what scenario
# 03 wants: pick one from between two rows to restore the configuration that was
# in force at that point.

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$HERE/config.sh"
. "$HERE/lib.sh"

command -v jq >/dev/null 2>&1 || die "jq is required by this script.  brew install jq"

server="${1:-$DEV_CLUSTER_NAME}"
shift 2>/dev/null || true
params="$*"
[ -n "$params" ] || params="shared_buffers work_mem max_connections"

prefix="${DEV_BUCKET}/${server}/chronicle/snapshots/"

run_rclone() {
  if command -v rclone >/dev/null 2>&1; then
    ( dev_rclone_export; rclone "$@" )
  else
    kubectl -n "$DEV_NAMESPACE" exec deploy/rclone -- rclone "$@"
  fi
}

keys=$(run_rclone lsf --files-only "dev:${prefix}" 2>/dev/null | sort) || true
[ -n "$keys" ] || die "no snapshots under s3://${prefix}"

printf '%-22s  %4s  %-10s  %s\n' "CAPTURED (UTC)" "GEN" "STORAGE" "$(echo "$params" | tr ' ' '/')"
printf '%-22s  %4s  %-10s  %s\n' "----------------------" "----" "----------" "----------------------------"

echo "$keys" | while read -r key; do
  [ -n "$key" ] || continue
  document=$(run_rclone cat "dev:${prefix}${key}" 2>/dev/null) || continue

  # One jq invocation per parameter. A handful more processes than strictly
  # necessary, for a dev environment that accumulates a handful of snapshots.
  values=""
  for p in $params; do
    v=$(printf '%s' "$document" | jq -r ".spec.postgresql.parameters.\"$p\" // \"-\"" 2>/dev/null) || v="-"
    values="$values$v  "
  done

  printf '%-22s  %4s  %-10s  %s\n' \
    "$(printf '%s' "$document" | jq -r '.capturedAt // "-"' | cut -c1-19)" \
    "$(printf '%s' "$document" | jq -r '.generation // "-"')" \
    "$(printf '%s' "$document" | jq -r '.spec.storage.size // "-"')" \
    "$values"
done

printf '\n%s%s snapshots. Read one in full with:  ./hack/dev/snapshot.sh %s | jq .%s\n' \
  "$C_DIM" "$(echo "$keys" | grep -c .)" "$server" "$C_RESET"
