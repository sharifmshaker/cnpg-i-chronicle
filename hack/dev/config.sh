# shellcheck shell=bash
# shellcheck disable=SC2034  # these are consumed by the scripts that source this
#
# Every knob the dev environment has, in one place.
#
# Each is overridable from the environment, so a one-off variation needs no edit
# here:  DEV_S3=seaweedfs ./hack/dev/up.sh

# --- cluster -----------------------------------------------------------------
KIND_CLUSTER="${KIND_CLUSTER:-chronicle-dev}"
# `kind load` takes the cluster name; kubectl takes the context, which kind
# prefixes with "kind-". Conflating the two is the classic kind foot-gun.
KUBE_CONTEXT="kind-${KIND_CLUSTER}"

# --- versions ----------------------------------------------------------------
# CNPG is resolved from the main branch at run time (see resolve_cnpg_version).
# Pin CNPG_VERSION to override, e.g. CNPG_VERSION=1.30.0
CNPG_VERSION="${CNPG_VERSION:-}"
# The release manifests carry a released operator image. Set CNPG_IMAGE to run a
# different build behind those manifests — notably
# ghcr.io/cloudnative-pg/cloudnative-pg-testing:main, which is how to exercise a
# change that has not reached a release yet.
CNPG_IMAGE="${CNPG_IMAGE:-}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.21.1}"
BARMAN_PLUGIN_VERSION="${BARMAN_PLUGIN_VERSION:-v0.14.0}"

# --- object store ------------------------------------------------------------
# rustfs (default) or seaweedfs. MinIO is deliberately absent: the project was
# archived in 2026 and its last published image predates a known CVE.
DEV_S3="${DEV_S3:-rustfs}"
DEV_BUCKET="${DEV_BUCKET:-chronicle}"
DEV_ACCESS_KEY="${DEV_ACCESS_KEY:-chronicle}"
DEV_SECRET_KEY="${DEV_SECRET_KEY:-chronicle123}"
DEV_S3_SERVICE="${DEV_S3_SERVICE:-objectstore}"

# Host ports, published by the kind node via extraPortMappings in kind.yaml.
# Changing these means changing kind.yaml too, and recreating the cluster.
DEV_S3_HOST_PORT="${DEV_S3_HOST_PORT:-19000}"
DEV_CONSOLE_HOST_PORT="${DEV_CONSOLE_HOST_PORT:-19001}"

case "$DEV_S3" in
  rustfs)    DEV_S3_IMAGE="${DEV_S3_IMAGE:-rustfs/rustfs:1.0.0-rc.3}" ;;
  seaweedfs) DEV_S3_IMAGE="${DEV_S3_IMAGE:-chrislusf/seaweedfs:4.44}" ;;
  *) echo "DEV_S3 must be 'rustfs' or 'seaweedfs', got '$DEV_S3'" >&2; exit 1 ;;
esac

DEV_RCLONE_IMAGE="${DEV_RCLONE_IMAGE:-rclone/rclone:1.75}"

# --- workload ----------------------------------------------------------------
DEV_NAMESPACE="${DEV_NAMESPACE:-default}"

# The in-cluster endpoint, fully qualified.
#
# It must be, and this is easy to get wrong: the plugin runs in the operator's
# namespace (cnpg-system) while the store lives beside the clusters, so a bare
# Service name does not resolve from where the S3 client actually runs. The
# symptom is "lookup objectstore ... no such host" in the plugin log while the
# very same name works fine from a pod in the store's own namespace.
DEV_S3_ENDPOINT="http://${DEV_S3_SERVICE}.${DEV_NAMESPACE}.svc.cluster.local:9000"
# The source cluster every scenario restores from.
#
# The scenarios in hack/dev/scenarios/ are applied with plain `kubectl apply`,
# with no substitution, so they name this cluster literally. Overriding it here
# gives you a differently-named cluster that those files will not find.
DEV_CLUSTER_NAME="${DEV_CLUSTER_NAME:-pg-source}"
DEV_PG_IMAGE="${DEV_PG_IMAGE:-ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie}"

# --- the plugin under test ---------------------------------------------------
IMG="${IMG:-ghcr.io/sharifmshaker/cnpg-i-chronicle:dev}"

# How to reach the dev store from the host.
#
# Path-style addressing is mandatory: virtual-host addressing (bucket.host)
# needs per-bucket DNS that a self-hosted store does not provide.
#
# Set as real variables rather than emitted as text for a caller to re-split on
# whitespace — a secret containing a space would silently become two arguments.
dev_rclone_export() {
  # The whole remote is defined by these variables, so point rclone at an empty
  # config file: without this it prints a NOTICE about the missing
  # ~/.config/rclone/rclone.conf before the output of every single command.
  export RCLONE_CONFIG="/dev/null"
  export RCLONE_CONFIG_DEV_TYPE="s3"
  export RCLONE_CONFIG_DEV_PROVIDER="Other"
  export RCLONE_CONFIG_DEV_ENDPOINT="http://localhost:${DEV_S3_HOST_PORT}"
  export RCLONE_CONFIG_DEV_ACCESS_KEY_ID="${DEV_ACCESS_KEY}"
  export RCLONE_CONFIG_DEV_SECRET_ACCESS_KEY="${DEV_SECRET_KEY}"
  export RCLONE_CONFIG_DEV_FORCE_PATH_STYLE="true"
}

# The same settings as `export` lines for eval, with every value shell-quoted.
dev_rclone_env() {
  dev_rclone_export
  for name in RCLONE_CONFIG RCLONE_CONFIG_DEV_TYPE RCLONE_CONFIG_DEV_PROVIDER \
              RCLONE_CONFIG_DEV_ENDPOINT RCLONE_CONFIG_DEV_ACCESS_KEY_ID \
              RCLONE_CONFIG_DEV_SECRET_ACCESS_KEY RCLONE_CONFIG_DEV_FORCE_PATH_STYLE; do
    # Indirect expansion, and every value single-quoted with embedded quotes
    # escaped, so a credential containing a space or a $ survives the eval.
    printf "export %s='%s'\n" "$name" "$(printf '%s' "${!name}" | sed "s/'/'\\\\''/g")"
  done
}
