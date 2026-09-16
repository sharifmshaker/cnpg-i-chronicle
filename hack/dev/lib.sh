# shellcheck shell=bash
#
# Shared helpers for the dev-environment scripts.
#
# Targets bash 3.2, which is what macOS ships. No associative arrays, no
# mapfile, no ${var,,} — all of those are bash 4.

set -euo pipefail

# ---------------------------------------------------------------- appearance

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'; C_DIM=$'\033[2m'
  C_BLUE=$'\033[34m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'
else
  C_RESET=""; C_BOLD=""; C_DIM=""; C_BLUE=""; C_GREEN=""; C_YELLOW=""; C_RED=""
fi

STEP_NUMBER=0

step()  { STEP_NUMBER=$((STEP_NUMBER + 1)); printf '\n%s==> [%d/%d] %s%s\n' "$C_BOLD$C_BLUE" "$STEP_NUMBER" "${TOTAL_STEPS:-?}" "$*" "$C_RESET"; }
info()  { printf '    %s\n' "$*"; }
dim()   { printf '    %s%s%s\n' "$C_DIM" "$*" "$C_RESET"; }
ok()    { printf '    %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn()  { printf '    %s!%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
die()   { printf '\n%serror:%s %s\n' "$C_RED$C_BOLD" "$C_RESET" "$*" >&2; exit 1; }

# ---------------------------------------------------------------- waiting

# retry <attempts> <delay-seconds> <description> -- <command...>
#
# Kubernetes objects frequently do not exist the instant the thing that creates
# them returns. Rather than sprinkling `sleep 10` around, every such spot gets a
# bounded retry with a description that names what is being waited for, so a
# timeout says which step failed rather than just exiting non-zero.
retry() {
  attempts=$1; delay=$2; description=$3; shift 4
  i=1
  waiting=no
  while [ "$i" -le "$attempts" ]; do
    if "$@" >/dev/null 2>&1; then
      # Close the dotted progress line if one was started. The caller prints its
      # own success message, so there is nothing to add here beyond the newline.
      [ "$waiting" = yes ] && printf '\n'
      return 0
    fi
    if [ "$waiting" = no ]; then
      printf '    waiting for %s' "$description"; waiting=yes
    else
      printf '.'
    fi
    sleep "$delay"
    i=$((i + 1))
  done
  printf '\n'
  die "timed out waiting for $description (${attempts} attempts, ${delay}s apart).
    Investigate with:  kubectl get pods -A"
}

# Wait for a Deployment to become Available. kubectl's own --for=condition
# fails outright if the object does not exist yet, so existence is waited for
# separately first.
wait_deploy() {
  namespace=$1; name=$2; timeout=${3:-300s}
  retry 60 5 "deployment/$name in $namespace to appear" -- \
    kubectl -n "$namespace" get deploy "$name"
  kubectl -n "$namespace" rollout status "deploy/$name" --timeout="$timeout" >/dev/null \
    || die "deployment/$name in $namespace never became ready.
    Investigate with:  kubectl -n $namespace describe deploy/$name
                       kubectl -n $namespace logs deploy/$name"
  ok "deployment/$name is ready"
}

# ---------------------------------------------------------------- running

# run <description> -- <command...>
#
# Quiet on success, and on failure prints what the command actually said before
# dying. Telling someone to "re-run it to see why" wastes the minutes they
# already spent waiting for it.
run() {
  description=$1; shift 2
  output=$(mktemp -t chronicle-dev)
  if "$@" >"$output" 2>&1; then
    rm -f "$output"
    return 0
  fi
  printf '\n'
  warn "$description failed. Its output:"
  sed 's/^/      /' "$output" | tail -25 >&2
  rm -f "$output"
  die "$description failed."
}

# ---------------------------------------------------------------- kubectl

# Apply a manifest with envsubst-style substitution of the dev settings, so the
# YAML files stay readable and single-sourced from config.sh.
apply_template() {
  file=$1
  substitute < "$file" | kubectl apply --server-side -f - >/dev/null
}

# A deliberately tiny substituter: only the ${NAME} forms this project uses.
# Using sed rather than envsubst keeps the dependency list to what macOS ships.
substitute() {
  sed \
    -e "s|\${DEV_NAMESPACE}|${DEV_NAMESPACE}|g" \
    -e "s|\${DEV_BUCKET}|${DEV_BUCKET}|g" \
    -e "s|\${DEV_ACCESS_KEY}|${DEV_ACCESS_KEY}|g" \
    -e "s|\${DEV_SECRET_KEY}|${DEV_SECRET_KEY}|g" \
    -e "s|\${DEV_S3_ENDPOINT}|${DEV_S3_ENDPOINT}|g" \
    -e "s|\${DEV_S3_SERVICE}|${DEV_S3_SERVICE}|g" \
    -e "s|\${DEV_S3_IMAGE}|${DEV_S3_IMAGE}|g" \
    -e "s|\${DEV_RCLONE_IMAGE}|${DEV_RCLONE_IMAGE}|g" \
    -e "s|\${DEV_CLUSTER_NAME}|${DEV_CLUSTER_NAME}|g" \
    -e "s|\${DEV_PG_IMAGE}|${DEV_PG_IMAGE}|g" \
    -e "s|\${IMG}|${IMG}|g"
}
