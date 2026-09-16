#!/usr/bin/env bash
#
# Rebuild the plugin from the working tree and restart it in the running
# environment. The fast inner loop: ~30s warm, against ~4 minutes for a full
# `up.sh`.
#
#   ./hack/dev/reload.sh

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
. "$HERE/config.sh"
. "$HERE/lib.sh"

# shellcheck disable=SC2034  # read by step() in lib.sh
TOTAL_STEPS=3

kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER" \
  || die "no kind cluster named '$KIND_CLUSTER'. Run ./hack/dev/up.sh first."

step "Rebuilding $IMG"
run "the image build" -- make -C "$ROOT" docker-build IMG="$IMG"
ok "built"

step "Loading it onto the node"
kind load docker-image "$IMG" --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
node="${KIND_CLUSTER}-control-plane"
if ! docker exec "$node" crictl images 2>/dev/null | grep -q "$(echo "$IMG" | cut -d: -f1)"; then
  docker save "$IMG" | docker exec -i "$node" ctr --namespace k8s.io images import - >/dev/null 2>&1 \
    || die "could not get $IMG onto the node."
fi
ok "loaded"

step "Restarting and re-applying manifests"
run "applying the plugin manifests" -- make -C "$ROOT" deploy
kubectl --context "$KUBE_CONTEXT" -n cnpg-system rollout restart deploy/chronicle >/dev/null
wait_deploy cnpg-system chronicle

printf '\n  %sReady.%s  kubectl -n cnpg-system logs -f deploy/chronicle\n\n' "$C_GREEN$C_BOLD" "$C_RESET"
