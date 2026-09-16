#!/usr/bin/env bash
#
# Delete the dev environment.
#
#   ./hack/dev/down.sh          # delete the whole kind cluster
#   ./hack/dev/down.sh --keep   # keep the cluster, remove only the workload
#
# The default is the whole cluster because that is the honest reset: it takes
# the object store's volume with it, so the next `up.sh` starts from an empty
# bucket rather than one holding snapshots from a previous shape of the code.

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$HERE/config.sh"
. "$HERE/lib.sh"

if [ "${1:-}" = "--keep" ]; then
  info "removing scenario objects, keeping the environment"

  # Everything except the environment's own source cluster.
  #
  # Deleting that one and recreating it under the same name is a trap: its WAL
  # archive survives in the bucket, CloudNativePG sees a non-empty archive for
  # that serverName, concludes the cluster is already bootstrapped, and refuses
  # to initialise it — leaving a Cluster stuck "unrecoverable" with no PVC and
  # no instance. Recreating it needs the archive cleared first, which this flag
  # exists to avoid having to do.
  for object in $(kubectl --context "$KUBE_CONTEXT" -n "$DEV_NAMESPACE" \
      get cluster.postgresql.cnpg.io -o name 2>/dev/null); do
    name=${object##*/}
    [ "$name" = "$DEV_CLUSTER_NAME" ] && continue
    kubectl --context "$KUBE_CONTEXT" -n "$DEV_NAMESPACE" delete "$object" >/dev/null 2>&1 || true
    info "deleted cluster $name"
  done

  kubectl --context "$KUBE_CONTEXT" -n "$DEV_NAMESPACE" delete \
    restorepolicy --all --ignore-not-found >/dev/null 2>&1 || true

  ok "scenario clusters and restore policies removed"
  dim "kept: $DEV_CLUSTER_NAME, the stores, and the bucket"
  exit 0
fi

if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
  info "deleting kind cluster '$KIND_CLUSTER'"
  kind delete cluster --name "$KIND_CLUSTER"
  ok "deleted"
else
  ok "no kind cluster named '$KIND_CLUSTER'; nothing to do"
fi
