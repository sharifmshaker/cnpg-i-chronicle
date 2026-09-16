# Manual testing

What to do, and what to expect, to exercise every phase of the plugin by hand.

> **Start with `make dev-up`.** It provisions everything these procedures need —
> see [`DEV-ENVIRONMENT.md`](DEV-ENVIRONMENT.md) — and every command below is
> written against the environment it builds: the kind context
> `kind-chronicle-dev`, the source cluster `pg-source`, the store `config-store`,
> and a RustFS bucket named `chronicle`. The scenarios in `hack/dev/scenarios/`
> demonstrate the same behaviour in one `kubectl apply` each; this document is
> the checklist behind them.

Everything below has been run end to end on macOS with colima. Where a step has
a way of going wrong that is not obvious, the symptom is written next to it.

- [Prerequisites](#prerequisites)
- [Assembling the environment by hand](#assembling-the-environment-by-hand)
- [Inspecting the bucket](#inspecting-the-bucket)
- [Test procedures](#test-procedures)
- [Troubleshooting](#troubleshooting)
- [Teardown](#teardown)

---

## Prerequisites

| Tool | Why | Install |
|---|---|---|
| `docker` | runs the kind node and builds the image | — |
| `docker buildx` | **required** to build the image; see note below | `brew install docker-buildx` |
| `kind` | the Kubernetes cluster | `brew install kind` |
| `kubectl` | everything | `brew install kubectl` |
| `rclone` | reading the bucket from the host | `brew install rclone` |
| `jq` | reading snapshots and restore records | `brew install jq` |
| `go` 1.26+ | tests, code generation | — |

`docker buildx` also needs registering as a CLI plugin on macOS, or `docker
buildx version` fails even after installing:

```jsonc
// ~/.docker/config.json
{
  "cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]
}
```

Without BuildKit, the Dockerfile's Go build-cache mounts are ignored and every
image build recompiles cel-go, controller-runtime and the Kubernetes libraries
from source in a fresh container: **11 minutes instead of 30 seconds.**

Raise the inotify limits in the Docker VM before creating a cluster. A low limit
makes kube-proxy crash-loop and breaks every ClusterIP, which looks nothing like
its cause; [`DEV-ENVIRONMENT.md`](DEV-ENVIRONMENT.md#raise-the-inotify-limits-first)
has the commands.

---

## Assembling the environment by hand

`make dev-up` does all of this, idempotently. Do it by hand when you need to
vary a step, or to debug the script. Each step matches one in `hack/dev/up.sh`,
and [`DEV-ENVIRONMENT.md`](DEV-ENVIRONMENT.md) explains why it is there.

```bash
kind create cluster --config hack/dev/kind.yaml --name chronicle-dev

kubectl apply --server-side -f \
  https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/main/releases/cnpg-1.30.0.yaml

# Needed for derivedFrom and for WAL archiving; the standalone store works without it.
kubectl apply --server-side -f \
  https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/v0.14.0/manifest.yaml
```

Wait for cert-manager's webhook to have an endpoint before applying anything
that creates a `Certificate`; until then every such apply is rejected.

The dev manifests use `${VAR}` placeholders, filled from `hack/dev/config.sh`.
Render and apply them the way `up.sh` does:

```bash
render() { ( . hack/dev/config.sh && . hack/dev/lib.sh && substitute < "$1" ); }

render hack/dev/manifests/objectstore-rustfs.yaml | kubectl apply --server-side -f -
render hack/dev/manifests/toolbox.yaml            | kubectl apply --server-side -f -   # rclone, and the bucket
```

Then the plugin:

```bash
make docker-build            # ~30s warm, ~11m cold
kind load docker-image ghcr.io/sharifmshaker/cnpg-i-chronicle:dev --name chronicle-dev
make deploy
kubectl -n cnpg-system rollout status deploy/chronicle
```

And the stores and source cluster:

```bash
render hack/dev/manifests/stores.yaml  | kubectl apply --server-side -f -
render hack/dev/manifests/cluster.yaml | kubectl apply --server-side -f -
```

### If `kind load` silently does nothing

Under colima's containerd image store, `kind load docker-image` can report
success and copy nothing. The symptom is `ImagePullBackOff` for an image you can
see in `docker images`. Stream it in instead:

```bash
docker save ghcr.io/sharifmshaker/cnpg-i-chronicle:dev \
  | docker exec -i chronicle-dev-control-plane ctr --namespace k8s.io images import -

docker exec chronicle-dev-control-plane crictl images | grep chronicle   # confirm
```

### Health check

```bash
kubectl -n cnpg-system logs deploy/chronicle --tail=20 | grep -oE '"msg":"[^"]*"'
kubectl get configstore
```

Expected on startup: `Registering webhook`, `Starting webhook server`,
`Starting plugin listener`, `Serving webhook server`, `Starting Controller`.
`config-store` should report `READY True`, which means its credentials were read
and the bucket answered a listing.

There is **no** leader-election step — the plugin does not use one. The
comments in `kubernetes/deployment.yaml` say why.

---

## Inspecting the bucket

The dev store is published on `localhost:19000`. Point rclone at it once per
shell:

```bash
eval $(./hack/dev/env.sh)

rclone tree dev:chronicle                                   # everything
rclone ls   dev:chronicle/pg-source/chronicle/snapshots/    # snapshots
rclone cat  dev:chronicle/pg-source/chronicle/latest.json | jq

./hack/dev/history.sh pg-source      # the history as a table
./hack/dev/snapshot.sh | jq          # the newest snapshot
```

Without host rclone, the same through the in-cluster pod:

```bash
kubectl exec deploy/rclone -- rclone tree dev:chronicle
```

---

## Test procedures

Each phase is independent. `./hack/dev/down.sh --keep` removes what a phase
created while keeping `pg-source`, the stores and the bucket.

### Phase 1 — capture

`pg-source` already captures into `config-store`.

| # | Do | Expect |
|---|---|---|
| 1.1 | `./hack/dev/history.sh pg-source` | at least one snapshot |
| 1.2 | change a GUC | a new snapshot; generation advances, new `checksum` |
| 1.3 | `kubectl label cluster pg-source tier=gold` | a new snapshot with **generation unchanged** — labels do not bump `metadata.generation`, so the metadata fingerprint is what catches it |
| 1.4 | change a field no default group captures, e.g. `enableSuperuserAccess` | generation advances, **no new snapshot** — the extracted checksum matches what the store already holds |
| 1.5 | leave it idle 2 minutes | `resourceVersion` and log line count static — no requeue storm |

```bash
# 1.2
kubectl patch cluster pg-source --type=merge \
  -p '{"spec":{"postgresql":{"parameters":{"shared_buffers":"128MB"}}}}'
./hack/dev/history.sh pg-source
```

`shared_buffers` must not exceed `resources.requests.memory`, or CNPG's own
validating webhook rejects the patch — a real constraint, not a plugin problem.

Also confirm the snapshot never contains archiving configuration:

```bash
rclone cat dev:chronicle/pg-source/chronicle/latest.json \
  | jq -r 'if (.spec | has("plugins") or has("backup") or has("externalClusters") or has("bootstrap")) then "LEAK" else "clean" end'
```

### Phase 2 — restore

```bash
kubectl apply -f - <<'EOF'
apiVersion: chronicle.sharifmshaker.github.io/v1
kind: RestorePolicy
metadata: {name: from-pg-source}
spec:
  select: {mode: Latest}
  skip:
    - group: Resources
    - path: metadata.annotations
      except: [example.com/owner]
EOF
```

| # | Do | Expect |
|---|---|---|
| 2.1 | `kubectl get restorepolicy` | `Ready: True` — a statement about the rules; the policy reads no store |
| 2.2 | create a cluster restoring from `pg-source`, asking for **more** storage than the source | persisted spec shows the source's smaller size — an update could never shrink it |
| 2.3 | read the restore record | one annotation key, `chronicle.sharifmshaker.github.io/restored-from`, not nested objects |
| 2.4 | read the restored cluster's annotations | `example.com/owner` restored from the source, and none of the source's other annotations |
| 2.5 | name a `restorePolicy` that does not exist | `kubectl apply` **denied**, message names the missing policy |
| 2.6 | set `saveToStore` and `saveToServer` equal to the source | denied: would interleave the source's history |
| 2.7 | delete the `MutatingWebhookConfiguration`, then create a restoring cluster | cluster refused, `PhaseFailurePlugin`, no pods created |

```bash
# 2.3
kubectl get cluster pg-restored \
  -o jsonpath='{.metadata.annotations.chronicle\.sharifmshaker\.github\.io/restored-from}' | jq
```

### Phase 3 — alignment with a recovery target

Restoring data to an earlier moment must bring the configuration that was live
then. Change a GUC between two captures so the history has something to
distinguish.

| # | Cluster sets | Expect |
|---|---|---|
| 3.1 | `recoveryTarget.targetTime` between two captures | the **earlier** snapshot's values |
| 3.2 | recovery with no target | the newest snapshot |
| 3.3 | `recoveryTarget.backupID` matching a `Backup` resource | that backup's `status.stoppedAt` |
| 3.4 | `targetXID` **with** `backupID` | allowed; `alignedWith` flags it approximate |
| 3.5 | `targetLSN` with no `backupID` | denied, unless `onUnresolvable: UseLatest` |

CloudNativePG **requires** `backupID` alongside `targetXID`, `targetName` and
`targetImmediate`; a cluster with `targetXID` alone is rejected by CNPG itself
before the plugin sees it.

`hack/dev/scenarios/04-restore-pitr.yaml` takes a real backup. To test 3.3
without one, create a `Backup` and set its status directly:

```bash
kubectl apply -f - <<'EOF'
apiVersion: postgresql.cnpg.io/v1
kind: Backup
metadata: {name: nightly}
spec:
  cluster: {name: pg-source}
  method: barmanObjectStore
EOF

kubectl patch backup nightly --subresource=status --type=merge \
  -p '{"status":{"backupId":"20260826T025730","stoppedAt":"2026-08-26T02:57:30Z","phase":"completed"}}'
```

```bash
kubectl get cluster pg-aligned \
  -o jsonpath='{.metadata.annotations.chronicle\.sharifmshaker\.github\.io/restored-from}' \
  | jq '{alignedWith, snapshotKey}'
```

### Phase 4 — transforms

```bash
kubectl apply -f - <<'EOF'
apiVersion: chronicle.sharifmshaker.github.io/v1
kind: RestorePolicy
metadata: {name: scaled}
spec:
  transform:
    - path: spec.storage.size
      expression: 'old.multiply(4)'
    - path: spec.resources.requests.memory
      expression: 'old.multiply(2)'
    - path: spec.postgresql.parameters.shared_buffers
      expression: 'pgScale(old, 2)'
    - path: spec.postgresql.parameters.max_connections
      expression: 'old > 100 ? 100 : old'
EOF

kubectl get restorepolicy scaled -o jsonpath='{.status.conditions}' | jq
```

| # | Do | Expect |
|---|---|---|
| 4.1 | read `status.conditions` | `Ready: True` — paths resolve against the allowlist and CloudNativePG's types |
| 4.2 | restore a cluster | values scaled, not copied; `max_connections` is the **string** `"100"`, because GUCs are strings and a result keeps the captured value's type |
| 4.3 | mistype a transform path | `Ready: False` naming the field that does not exist; cluster creation denied with the same message |
| 4.4 | transform a path also in `skip` | denied as contradictory |
| 4.5 | `old.multiply(2)` on a GUC whose value is `on` | `Ready: True` (the path is valid), then the cluster is denied — type errors surface only at evaluation |

**Quote every expression.** A ternary contains a colon, so unquoted
`old > 1 ? 1 : old` parses as a YAML mapping, not a string.

Transforms can produce individually valid but **jointly invalid** values —
scaling memory down while scaling `shared_buffers` up. CNPG's validating webhook
catches that and refuses the cluster. This plugin checks each rule in isolation
and deliberately does not duplicate CNPG's cross-field rules.

### Regression checks worth repeating

| Check | Why |
|---|---|
| plugin starts with the barman CRD **absent** | the standalone `configuration` path must not depend on it |
| plugin picks up a barman `ObjectStore` created **after** it started | reads are uncached, no informer |
| a `ConfigStore` naming a missing Secret key is `Ready: False` | credentials are read when the store is checked, not first at capture |
| idle 2 minutes: no new log lines, `resourceVersion` static | CloudNativePG polls the plugin's status every 5s; a settled cluster must answer without provoking a write |
| restart the plugin, confirm no re-capture | the watermark lives in the bucket, not in the process |

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| every controller times out on `https://10.96.0.1:443/api` | `kube-proxy` crash-looping on inotify limits | raise `fs.inotify.max_user_instances` and delete the `kube-proxy` pods |
| `ImagePullBackOff` for the plugin image you built | `kind load` did not take | stream the image in; see [above](#if-kind-load-silently-does-nothing) |
| image build takes 11+ minutes every time | BuildKit missing, cache mounts ignored | install `docker-buildx` and register `cliPluginsExtraDirs` |
| `Memory request is lower than shared_buffers` | CNPG cross-field validation | raise `resources.requests.memory` or lower `shared_buffers` |
| CNPG operator restarting repeatedly | CPU or memory starvation on a small VM | delete unused test clusters; do not build images while testing |
| cluster stuck in `PhaseUnknownPlugin` | plugin Service not discovered | it must be in the operator's namespace with the `cnpg.io/pluginName` label and all three `cnpg.io/plugin*` annotations |
| `mapping values are not allowed in this context` applying a policy | unquoted CEL ternary | quote the expression |

Useful one-liners:

```bash
# plugin messages without the stack traces
kubectl -n cnpg-system logs deploy/chronicle --tail=50 | grep -oE '"(msg|error)":"[^"]{0,200}"'

# why a cluster is unhappy
kubectl get cluster pg-source -o jsonpath='{.status.phaseReason}'

# what the plugin has captured, read straight from the store
./hack/dev/history.sh pg-source
```

---

## Teardown

```bash
./hack/dev/down.sh --keep    # remove what the phases created, keep the environment
make dev-down                # delete the whole kind cluster, bucket included

docker buildx prune          # only if you want the ~11 minute cold build back
```
