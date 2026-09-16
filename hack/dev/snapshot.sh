#!/usr/bin/env bash
#
# Print the newest configuration snapshot for a cluster, as JSON.
#
#   ./hack/dev/snapshot.sh              # the default dev cluster
#   ./hack/dev/snapshot.sh pg-restored  # some other server name
#   ./hack/dev/snapshot.sh | jq .spec.postgresql
#
# Snapshot keys are content-addressed and start with a UTC timestamp, so lexical
# order is chronological and `sort | tail -1` is genuinely the newest.

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$HERE/config.sh"
. "$HERE/lib.sh"

server="${1:-$DEV_CLUSTER_NAME}"
prefix="${DEV_BUCKET}/${server}/chronicle/snapshots/"

run_rclone() {
  if command -v rclone >/dev/null 2>&1; then
    ( dev_rclone_export; rclone "$@" )
  else
    kubectl -n "$DEV_NAMESPACE" exec deploy/rclone -- rclone "$@"
  fi
}

newest=$(run_rclone lsf --files-only "dev:${prefix}" 2>/dev/null | sort | tail -1) || true
[ -n "$newest" ] || die "no snapshots under s3://${prefix}
    Has the cluster been created, and is it configured with saveTo?
    Check:  kubectl -n $DEV_NAMESPACE get configstore config-store -o yaml"

run_rclone cat "dev:${prefix}${newest}"
