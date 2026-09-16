#!/usr/bin/env bash
#
# Bring up a complete chronicle development environment in one shot.
#
#   ./hack/dev/up.sh
#
# Creates a kind cluster and installs, in dependency order: cert-manager, the
# CloudNativePG operator from the main branch, the barman-cloud plugin, an
# S3-compatible object store, this plugin built from the working tree, and a
# PostgreSQL cluster wired to all of it. Finishes by printing the commands to
# inspect what landed in the bucket.
#
# Re-running is safe. Every step is idempotent, so this doubles as the way to
# push a code change into a running environment (or use `make dev-reload`,
# which skips straight to rebuild-and-restart).
#
# See docs/DEV-ENVIRONMENT.md for what each step does and why.

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

# shellcheck source=hack/dev/config.sh
. "$HERE/config.sh"
# shellcheck source=hack/dev/lib.sh
. "$HERE/lib.sh"

# shellcheck disable=SC2034  # read by step() in lib.sh
TOTAL_STEPS=9

# ---------------------------------------------------------------------------
# 1. Preflight
# ---------------------------------------------------------------------------

preflight() {
  step "Checking prerequisites"

  for tool in docker kind kubectl; do
    command -v "$tool" >/dev/null 2>&1 \
      || die "'$tool' is required but not on PATH.
    macOS:  brew install $tool"
  done
  docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable. Start Docker (or colima) and retry."
  ok "docker, kind, kubectl"

  if command -v rclone >/dev/null 2>&1; then
    HAVE_RCLONE=yes
    ok "rclone $(rclone version 2>/dev/null | head -1 | awk '{print $2}') (host bucket access)"
  else
    HAVE_RCLONE=no
    warn "rclone is not installed; bucket inspection will use the in-cluster pod instead."
    dim  "brew install rclone  — for the shorter commands"
  fi

  check_host_ports

  # A low inotify ceiling is the single most common cause of a mysteriously
  # broken kind cluster: kube-proxy crash-loops, and every ClusterIP in the
  # cluster stops resolving. The symptom looks nothing like the cause.
  if command -v docker >/dev/null 2>&1; then
    instances=$(docker run --rm --privileged --pid=host alpine:3 \
      nsenter -t 1 -m -u -n -i sysctl -n fs.inotify.max_user_instances 2>/dev/null || echo "")
    if [ -n "$instances" ] && [ "$instances" -lt 512 ] 2>/dev/null; then
      warn "fs.inotify.max_user_instances is $instances inside the Docker VM (want >= 512)."
      dim  "kube-proxy may crash-loop, which breaks every ClusterIP in the cluster."
      dim  "colima:  colima ssh -- sudo sysctl -w fs.inotify.max_user_instances=512"
    fi
  fi
}

# kind publishes the object store's ports on the host when it creates the node,
# so a conflict surfaces as a wall of docker output from deep inside `kind
# create cluster`. Catching it here turns that into a sentence naming the
# offending process.
#
# Skipped when our own cluster already exists, because then the container
# holding those ports is our node and a re-run must not refuse itself.
check_host_ports() {
  if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
    dim "cluster '$KIND_CLUSTER' already exists; leaving its published ports alone"
    return
  fi

  for port in "$DEV_S3_HOST_PORT" "$DEV_CONSOLE_HOST_PORT"; do
    holder=$(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | awk 'NR==2 {print $1" (pid "$2")"}') || true
    [ -n "$holder" ] || continue

    container=$(docker ps --format '{{.Names}}\t{{.Ports}}' 2>/dev/null \
      | grep ":${port}->" | cut -f1 | head -1) || true
    if [ -n "$container" ]; then
      die "port $port is already published by the container '$container'.

    Free it, then re-run:
        docker stop $container

    Or move this environment's ports (both config.sh and kind.yaml):
        DEV_S3_HOST_PORT=29000 DEV_CONSOLE_HOST_PORT=29001 ./hack/dev/up.sh"
    fi
    die "port $port is already in use by $holder.
    Free it, or pick different ports in hack/dev/config.sh and hack/dev/kind.yaml."
  done
  ok "host ports $DEV_S3_HOST_PORT and $DEV_CONSOLE_HOST_PORT are free"
}

# ---------------------------------------------------------------------------
# 2. CNPG version, resolved from the main branch
# ---------------------------------------------------------------------------

resolve_cnpg_version() {
  step "Resolving the CloudNativePG version"
  if [ -n "$CNPG_VERSION" ]; then
    ok "CloudNativePG pinned to $CNPG_VERSION"
    return
  fi

  # The operator manifests live in releases/ on main. Take the newest stable
  # one rather than pinning, so the environment tracks main as it moves; an
  # unreachable or rate-limited API falls back to a known-good version instead
  # of failing the whole setup.
  CNPG_VERSION=$(
    curl -sfL --max-time 15 \
      https://api.github.com/repos/cloudnative-pg/cloudnative-pg/contents/releases 2>/dev/null \
      | grep -o '"name": "cnpg-[0-9.]*\.yaml"' \
      | sed -e 's/.*cnpg-//' -e 's/\.yaml"//' \
      | sort -t. -k1,1n -k2,2n -k3,3n \
      | tail -1
  ) || true

  if [ -z "$CNPG_VERSION" ]; then
    CNPG_VERSION="1.30.0"
    warn "could not reach the GitHub API; falling back to CloudNativePG $CNPG_VERSION"
  else
    ok "CloudNativePG $CNPG_VERSION (newest on main)"
  fi
}

# ---------------------------------------------------------------------------
# 3. The kind cluster
# ---------------------------------------------------------------------------

create_cluster() {
  step "Creating the kind cluster '$KIND_CLUSTER'"

  if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
    ok "cluster already exists (reusing it)"
  else
    kind create cluster --config "$HERE/kind.yaml" --name "$KIND_CLUSTER" \
      || die "kind failed to create the cluster."
    ok "cluster created"
  fi

  kubectl config use-context "$KUBE_CONTEXT" >/dev/null 2>&1 \
    || die "no kubectl context named '$KUBE_CONTEXT'."
  retry 30 2 "the API server to answer" -- kubectl get --raw /healthz
  ok "context set to $KUBE_CONTEXT"
}

# ---------------------------------------------------------------------------
# 4-6. Operators
# ---------------------------------------------------------------------------

install_cert_manager() {
  step "Installing cert-manager $CERT_MANAGER_VERSION"
  # Required by both the barman plugin and chronicle: CNPG-I is mutual TLS, and
  # the certificates are issued by cert-manager rather than baked into images.
  kubectl apply --server-side -f \
    "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml" >/dev/null \
    || die "could not apply the cert-manager manifest."
  wait_deploy cert-manager cert-manager
  wait_deploy cert-manager cert-manager-webhook
  # The webhook being Available is not the same as it serving: until its
  # endpoint is reachable, every Certificate apply is rejected.
  retry 60 5 "the cert-manager webhook to accept traffic" -- \
    kubectl -n cert-manager get endpoints cert-manager-webhook \
      -o jsonpath='{.subsets[0].addresses[0].ip}'
}

install_cnpg() {
  step "Installing CloudNativePG $CNPG_VERSION"
  # --force-conflicts because this script is the only writer of this
  # Deployment, under two field managers. A previous run with CNPG_IMAGE set
  # the image through `kubectl set`, and without the flag every later apply
  # conflicts with that and fails, whether or not CNPG_IMAGE is set this time.
  # The manifest takes the fields back here, and the override below re-applies
  # them when it is still wanted.
  kubectl apply --server-side --force-conflicts -f \
    "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/main/releases/cnpg-${CNPG_VERSION}.yaml" >/dev/null \
    || die "could not apply the CloudNativePG manifest for $CNPG_VERSION."

  # The manifests are a release's; the image need not be. This is how to run an
  # operator carrying a fix that has not been released yet — see CNPG_IMAGE in
  # config.sh. CRDs and RBAC still come from the release, so this is only sound
  # for a build close to it.
  #
  # OPERATOR_IMAGE_NAME has to move with the container image. The operator does
  # not run the instance manager itself: it injects the image that variable
  # names as the bootstrap-controller init container, and every PostgreSQL pod
  # takes its instance manager binary from there. Overriding only the Deployment
  # leaves the controller on one version and every instance on another, which
  # here meant the operator probing the status port over HTTPS while a 1.30.0
  # instance manager answered plain HTTP — no instance ever passes its startup
  # probe, and the cluster never reports healthy.
  if [ -n "$CNPG_IMAGE" ]; then
    kubectl -n cnpg-system set image deployment/cnpg-controller-manager \
      manager="$CNPG_IMAGE" >/dev/null \
      || die "could not point the operator at $CNPG_IMAGE."
    kubectl -n cnpg-system set env deployment/cnpg-controller-manager \
      OPERATOR_IMAGE_NAME="$CNPG_IMAGE" >/dev/null \
      || die "could not set OPERATOR_IMAGE_NAME to $CNPG_IMAGE."
    ok "operator image overridden: $CNPG_IMAGE (controller and instances)"
  fi

  wait_deploy cnpg-system cnpg-controller-manager
}

install_barman_plugin() {
  step "Installing the barman-cloud plugin $BARMAN_PLUGIN_VERSION"
  # Brings its own ObjectStore CRD and its cert-manager Certificates.
  kubectl apply --server-side -f \
    "https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/${BARMAN_PLUGIN_VERSION}/manifest.yaml" >/dev/null \
    || die "could not apply the barman-cloud plugin manifest."
  wait_deploy cnpg-system barman-cloud
}

# ---------------------------------------------------------------------------
# 7. Object store
# ---------------------------------------------------------------------------

install_object_store() {
  step "Deploying the object store ($DEV_S3) and rclone toolbox"

  apply_template "$HERE/manifests/objectstore-${DEV_S3}.yaml"
  wait_deploy "$DEV_NAMESPACE" "$DEV_S3_SERVICE"

  # Delete any Job a previous run left behind before applying: a Job's pod
  # template is immutable once created, so re-applying an existing one fails.
  kubectl -n "$DEV_NAMESPACE" delete job create-bucket --ignore-not-found >/dev/null 2>&1
  apply_template "$HERE/manifests/toolbox.yaml"

  kubectl -n "$DEV_NAMESPACE" wait --for=condition=complete job/create-bucket --timeout=180s >/dev/null 2>&1 \
    || die "the bucket-creation job did not complete.
    Investigate with:  kubectl -n $DEV_NAMESPACE logs job/create-bucket"
  ok "bucket s3://${DEV_BUCKET}/ exists"
  wait_deploy "$DEV_NAMESPACE" rclone
}

# ---------------------------------------------------------------------------
# 8. The plugin under test
# ---------------------------------------------------------------------------

install_chronicle() {
  step "Building and deploying chronicle from the working tree"

  info "building $IMG (first run is slow; later ones hit the Go build cache)"
  run "the image build" -- make -C "$ROOT" docker-build IMG="$IMG"
  ok "image built"

  load_image
  run "applying the plugin manifests" -- make -C "$ROOT" deploy

  # A rollout restart, so a re-run with a rebuilt image of the same tag is
  # actually picked up rather than silently keeping the old process.
  kubectl -n cnpg-system rollout restart deploy/chronicle >/dev/null 2>&1 || true
  wait_deploy cnpg-system chronicle
}

# `kind load docker-image` can report success and do nothing when Docker is
# backed by a containerd image store, which is the default under colima. The
# failure is silent and the symptom is an ImagePullBackOff for an image you can
# see locally, so the load is verified and a working fallback used.
load_image() {
  info "loading $IMG into the kind node"
  kind load docker-image "$IMG" --name "$KIND_CLUSTER" >/dev/null 2>&1 || true

  node="${KIND_CLUSTER}-control-plane"
  if docker exec "$node" crictl images 2>/dev/null | grep -q "$(echo "$IMG" | cut -d: -f1)"; then
    ok "image present on the node"
    return
  fi

  warn "kind load did not take (containerd image store); falling back to a stream import"
  docker save "$IMG" | docker exec -i "$node" ctr --namespace k8s.io images import - >/dev/null 2>&1 \
    || die "could not get $IMG onto the kind node."
  ok "image imported"
}

# ---------------------------------------------------------------------------
# 9. Workload
# ---------------------------------------------------------------------------

# These three are shell functions rather than `sh -c "..."` strings so the
# jsonpath expressions do not have to survive two rounds of quoting. The cluster
# name reaches jsonpath in bracket notation because it contains a hyphen.
store_is_ready() {
  kubectl -n "$DEV_NAMESPACE" get configstore config-store \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null \
    | grep -q True
}

cluster_is_healthy() {
  kubectl -n "$DEV_NAMESPACE" get cluster "$DEV_CLUSTER_NAME" \
    -o jsonpath='{.status.phase}' 2>/dev/null \
    | grep -q 'Cluster in healthy state'
}

snapshot_was_captured() {
  # Asked of the object store, which is where a snapshot actually is, and which
  # is the source of truth regardless of what the Cluster's status says.
  local out
  if command -v rclone >/dev/null 2>&1; then
    out=$( dev_rclone_export
           rclone lsf --files-only \
             "dev:${DEV_BUCKET}/${DEV_CLUSTER_NAME}/chronicle/snapshots/" 2>/dev/null ) || return 1
  else
    out=$(kubectl -n "$DEV_NAMESPACE" exec deploy/rclone -- \
      rclone lsf --files-only \
      "dev:${DEV_BUCKET}/${DEV_CLUSTER_NAME}/chronicle/snapshots/" 2>/dev/null) || return 1
  fi
  [ -n "$out" ]
}

install_workload() {
  step "Creating the object stores and the PostgreSQL cluster"

  apply_template "$HERE/manifests/stores.yaml"
  retry 30 2 "ConfigStore config-store to resolve" -- store_is_ready
  ok "ConfigStore config-store resolved"

  apply_template "$HERE/manifests/cluster.yaml"
  info "waiting for $DEV_CLUSTER_NAME to come up (first run pulls the postgres image)"
  retry 90 10 "cluster $DEV_CLUSTER_NAME to report healthy" -- cluster_is_healthy
  ok "cluster $DEV_CLUSTER_NAME is healthy"

  # The run does not claim success until chronicle has actually captured
  # something. A cluster that comes up while the plugin silently does nothing is
  # the failure most worth catching here.
  retry 30 4 "the first configuration snapshot" -- snapshot_was_captured
  ok "chronicle captured a snapshot"
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

summary() {
  generation=$( dev_rclone_export
                rclone lsf --files-only \
                  "dev:${DEV_BUCKET}/${DEV_CLUSTER_NAME}/chronicle/snapshots/" 2>/dev/null \
                | sed -n 's/.*-g0*\([0-9][0-9]*\)-.*/\1/p' | tail -1 ) || true
  [ -n "$generation" ] || generation="?"

  printf '\n%s%s\n' "$C_BOLD$C_GREEN" "The environment is up.$C_RESET"
  cat <<EOF

  ${C_BOLD}What is running${C_RESET}
    kind cluster        ${KIND_CLUSTER}  (context: ${KUBE_CONTEXT})
    CloudNativePG       ${CNPG_VERSION}
    barman-cloud        ${BARMAN_PLUGIN_VERSION}
    object store        ${DEV_S3}  (${DEV_S3_IMAGE})
    PostgreSQL cluster  ${DEV_CLUSTER_NAME}  -> snapshots at generation ${generation}
    bucket              s3://${DEV_BUCKET}/

  ${C_BOLD}Browse the store${C_RESET}
    open http://localhost:${DEV_CONSOLE_HOST_PORT}
    ${C_DIM}user ${DEV_ACCESS_KEY} / password ${DEV_SECRET_KEY}${C_RESET}
EOF

  if [ "${HAVE_RCLONE:-no}" = "yes" ]; then
    cat <<EOF

  ${C_BOLD}Inspect the bucket${C_RESET} ${C_DIM}(host rclone, via localhost:${DEV_S3_HOST_PORT})${C_RESET}
    ${C_DIM}# point rclone at the dev store — once per shell${C_RESET}
    eval \$(./hack/dev/env.sh)

    ${C_DIM}# everything the cluster has written: backups and snapshots side by side${C_RESET}
    rclone tree dev:${DEV_BUCKET}

    ${C_DIM}# the configuration snapshots chronicle wrote${C_RESET}
    rclone ls dev:${DEV_BUCKET}/${DEV_CLUSTER_NAME}/chronicle/snapshots/

    ${C_DIM}# the WAL barman-cloud archived, in the same bucket${C_RESET}
    rclone ls dev:${DEV_BUCKET}/${DEV_CLUSTER_NAME}/wals/
    ${C_DIM}# base/ appears once a backup is taken — see scenario 04${C_RESET}

    ${C_DIM}# read the newest snapshot document${C_RESET}
    ./hack/dev/snapshot.sh | jq .
EOF
  else
    cat <<EOF

  ${C_BOLD}Inspect the bucket${C_RESET} ${C_DIM}(in-cluster rclone; no host tooling needed)${C_RESET}
    kubectl -n ${DEV_NAMESPACE} exec deploy/rclone -- rclone tree dev:${DEV_BUCKET}
    kubectl -n ${DEV_NAMESPACE} exec deploy/rclone -- rclone ls dev:${DEV_BUCKET}/${DEV_CLUSTER_NAME}/chronicle/snapshots/
    ${C_DIM}# or install rclone for the shorter form: brew install rclone${C_RESET}
EOF
  fi

  cat <<EOF

  ${C_BOLD}Watch the plugin${C_RESET}
    kubectl -n cnpg-system logs -f deploy/chronicle
    kubectl -n ${DEV_NAMESPACE} get configstore config-store -o yaml

  ${C_BOLD}Try a scenario${C_RESET} ${C_DIM}(each is a single kubectl apply)${C_RESET}
    kubectl apply -f hack/dev/scenarios/01-restore-basic.yaml
    ls hack/dev/scenarios/

  ${C_BOLD}Iterate${C_RESET}
    make dev-reload      ${C_DIM}# rebuild the plugin and restart it, ~30s${C_RESET}
    make dev-down        ${C_DIM}# delete the kind cluster${C_RESET}

EOF
}

# ---------------------------------------------------------------------------

main() {
  printf '%schronicle dev environment%s\n' "$C_BOLD" "$C_RESET"
  preflight
  resolve_cnpg_version
  create_cluster
  install_cert_manager
  install_cnpg
  install_barman_plugin
  install_object_store
  install_chronicle
  install_workload
  summary
}

main "$@"
