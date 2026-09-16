# Manual setup and testing

How to stand up a cluster, deploy the plugin, and exercise every phase by hand.

> **Most of the time you want `make dev-up` instead.** It provisions all of
> this automatically — see [`DEV-ENVIRONMENT.md`](DEV-ENVIRONMENT.md). This
> document is the manual procedure behind it: read it to understand a step, to
> reproduce something in a topology the script does not expose, or to debug the
> script itself.

Everything below has been run end to end on macOS with colima. Where a step has
a way of going wrong that is not obvious, the symptom is written next to it —
several of these cost hours to diagnose the first time.

## A note on MinIO

Earlier revisions of this document used MinIO, and the commands below still
reference it in places. **MinIO's community edition was archived during 2026** —
`minio/minio` in April, `minio/mc` in July — and its last published image
(September 2025) predates CVE-2025-62506, which was fixed only in source.

Do not start new work against it. The automated environment uses **RustFS** by
default (what cnpg-playground migrated to) with **SeaweedFS** as a one-variable
alternative, and **rclone** in place of `mc`. Where a MinIO command appears
below, the rclone equivalent is:

```bash
# was: mc alias set s <endpoint> <key> <secret> && mc ls --recursive s/<bucket>/
rclone --s3-provider Other --s3-endpoint <endpoint> \
  --s3-access-key-id <key> --s3-secret-access-key <secret> \
  --s3-force-path-style ls :s3:<bucket>/
```

Or, against the dev environment, simply:

```bash
eval $(./hack/dev/env.sh) && rclone tree dev:chronicle
```

- [Prerequisites](#prerequisites)
- [Environment](#environment)
- [Object store](#object-store)
- [Deploying the plugin](#deploying-the-plugin)
- [Test procedures](#test-procedures)
- [Troubleshooting](#troubleshooting)
- [Teardown](#teardown)

---

## Prerequisites

| Tool | Why | Install |
|---|---|---|
| `docker` (or podman) | runs the kind nodes and the object store | — |
| `docker buildx` | **required** to build the image; see note below | `brew install docker-buildx` |
| `kind` | the Kubernetes cluster | `brew install kind` |
| `kubectl` | everything | `brew install kubectl` |
| `kubectl cnpg` | `demo/setup.sh` calls it | `brew install kubectl-cnpg` |
| `cmctl` | `demo/setup.sh` waits on cert-manager with it | `brew install cmctl` |
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

### Raise the inotify limits first

Do this before creating any cluster. It is the single highest-value step here.

```bash
colima ssh -- sudo sysctl -w fs.inotify.max_user_instances=8192 \
                              fs.inotify.max_user_watches=1048576
```

A six-node kind cluster exhausts the default `max_user_instances=128`.
`kube-proxy` and CoreDNS then crash-loop with `too many open files`, which
breaks Service ClusterIP routing **cluster-wide**. The symptom is not a
networking error — it is every controller timing out on
`https://10.96.0.1:443/api`, which looks like a broken operator. It does not
survive a colima restart.

---

## Environment

### Option A — cnpg-playground

Realistic: multiple regions, tainted Postgres nodes across simulated zones, an
S3-compatible store per region. Note it uses **RustFS**, not MinIO.

```bash
git clone https://github.com/cloudnative-pg/cnpg-playground
cd cnpg-playground

./scripts/setup.sh eu                      # one region is enough for phases 1-4
export KUBECONFIG=$PWD/k8s/kube-config.yaml

REQUIREMENTS_ONLY=true ./demo/setup.sh eu  # CNPG + cert-manager, no demo clusters
```

Pass the region explicitly. With no argument the script auto-detects using
`mapfile`, which macOS's bash 3.2 does not have (`mapfile: command not found`).

`REQUIREMENTS_ONLY=true` skips creating demo Postgres clusters, which is what
you want when the thing under test is the plugin.

The kind cluster is `k8s-eu`; the kubectl context is `kind-k8s-eu`. `kind load`
wants the cluster name, `kubectl` wants the context — an easy hour to lose.

### Connecting to a cluster that is already running

The playground writes its kubeconfig into its own checkout rather than to
`~/.kube/config`, so **every new shell needs the export**. Without it `kubectl`
talks to whatever your default config points at, and the failure reads as if the
cluster were down rather than as if you were pointed elsewhere.

```bash
export KUBECONFIG=/absolute/path/to/cnpg-playground/k8s/kube-config.yaml
kubectl config use-context kind-k8s-eu
```

Use an absolute path. The setup snippet above writes `$PWD/k8s/kube-config.yaml`,
which only resolves while you are standing in the playground directory.

Confirm you are talking to the right thing:

```bash
kubectl config get-contexts          # kind-k8s-eu should be current
kind get clusters                    # k8s-eu
kubectl get nodes                    # 1 control-plane + 5 workers
```

Worth adding to your shell profile if you are iterating:

```bash
alias kpg='export KUBECONFIG=/absolute/path/to/cnpg-playground/k8s/kube-config.yaml'
```

To merge it into your default config instead of switching, so `kubectl` sees it
everywhere:

```bash
KUBECONFIG=~/.kube/config:/absolute/path/to/cnpg-playground/k8s/kube-config.yaml \
  kubectl config view --flatten > /tmp/merged && mv /tmp/merged ~/.kube/config
kubectl config use-context kind-k8s-eu
```

Back up `~/.kube/config` first — `--flatten` rewrites the whole file, and the
playground's `teardown.sh` prunes only its own entries from the file it created.

For a plain kind cluster (Option B) none of this applies: `kind create cluster`
writes to `~/.kube/config` and switches to it.

### Option B — plain kind

Lighter, if the multi-region topology is not what you are exercising.

```bash
kind create cluster --name k8s-eu
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.30/releases/cnpg-1.30.0.yaml
kubectl apply --server-side -f \
  https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml
```

Then create the object store yourself (see below) and attach it to the kind
network.

### Pod networking only works reliably on the control-plane node

In the playground, the CNPG operator, cert-manager and this plugin are all
pinned to the control-plane with a `nodeSelector` and a blanket toleration.
Keep it that way unless you have verified pod-to-apiserver routing from the
workers. A pod on a worker that cannot reach `10.96.0.1:443` fails in a way that
looks like a bug in whatever is running there.

---

## Object store

The plugin talks to **any S3-compatible endpoint**. A custom `endpointURL`
switches the client to path-style addressing, which self-hosted servers require
— virtual-host addressing needs per-bucket DNS they do not provide. Both stores
below are verified end to end.

The endpoint must be on the `kind` Docker network, or pods cannot reach it.

### RustFS (what the playground provides)

`./scripts/setup.sh` starts it and distributes credentials as a Secret named
`objectstore-<region>` in the `default` namespace, keys `ACCESS_KEY_ID` and
`ACCESS_SECRET_KEY`. Reachable in-cluster at `http://objectstore-eu:9000`; the
console is on the host at `localhost:9001`.

```yaml
apiVersion: chronicle.sharifmshaker.github.io/v1
kind: ConfigStore
metadata:
  name: config-store
spec:
  configuration:
    destinationPath: s3://backups/
    endpointURL: http://objectstore-eu:9000
    s3Credentials:
      accessKeyId:     {name: objectstore-eu, key: ACCESS_KEY_ID}
      secretAccessKey: {name: objectstore-eu, key: ACCESS_SECRET_KEY}
```

### MinIO

```bash
docker run -d --name chronicle-minio \
  -p 19000:9000 -p 19001:9001 \
  -e MINIO_ROOT_USER=chronicle -e MINIO_ROOT_PASSWORD=chronicle123 \
  quay.io/minio/minio:latest server /data --console-address ":9001"

docker network connect kind chronicle-minio   # without this, pods cannot reach it

kubectl create secret generic minio-creds \
  --from-literal=ACCESS_KEY_ID=chronicle \
  --from-literal=ACCESS_SECRET_KEY=chronicle123
```

```yaml
apiVersion: chronicle.sharifmshaker.github.io/v1
kind: ConfigStore
metadata:
  name: meta-minio
spec:
  configuration:
    destinationPath: s3://chronicle-e2e/
    endpointURL: http://chronicle-minio:9000
    s3Credentials:
      accessKeyId:     {name: minio-creds, key: ACCESS_KEY_ID}
      secretAccessKey: {name: minio-creds, key: ACCESS_SECRET_KEY}
```

Neither RustFS nor MinIO creates a bucket implicitly, and creating one by
`mkdir` in RustFS's data directory corrupts it. The plugin creates the bucket on
first write, matching barman-cloud; if `s3:CreateBucket` is denied it says so
and names the bucket.

### Deriving from a barman ObjectStore

To reuse a store already configured for backups, reference it instead of
restating it:

```yaml
spec:
  derivedFrom:
    name: objectstore-eu     # barmancloud.cnpg.io/v1
```

Only `.spec.configuration` is read. The barman **CRD** must be installed, but
the barman **plugin** need not be running — the CRD alone is enough:

```bash
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/plugin-barman-cloud/v0.14.0/config/crd/bases/barmancloud.cnpg.io_objectstores.yaml
```

### Inspecting the bucket

`mc` needs to be on the kind network too:

```bash
docker run --rm --network kind --entrypoint sh quay.io/minio/mc:latest -c \
  "mc alias set s http://objectstore-eu:9000 cnpg Cl0udNativePGRocks >/dev/null &&
   mc ls --recursive s/backups/"
```

---

## Deploying the plugin

```bash
make docker-build            # ~30s warm, ~11m cold
kind load docker-image ghcr.io/sharifmshaker/cnpg-i-chronicle:dev --name k8s-eu
make deploy
kubectl -n cnpg-system rollout status deploy/chronicle
```

### If `kind load` silently does nothing

Under colima's containerd image store, both `kind load docker-image` and
`docker cp` can report success and copy nothing. Pipe through `ctr` instead:

```bash
docker save ghcr.io/sharifmshaker/cnpg-i-chronicle:dev -o /tmp/img.tar
docker exec -i k8s-eu-control-plane \
  ctr -n k8s.io images import --platform linux/arm64 - < /tmp/img.tar

docker exec k8s-eu-control-plane crictl images | grep chronicle   # confirm
```

### Health check

```bash
kubectl -n cnpg-system logs -l app=chronicle --tail=20 | grep -oE '"msg":"[^"]*"'
```

Expected on startup: `Registering webhook`, `Starting webhook server`,
`Starting plugin listener`, `Serving webhook server`, `Starting Controller`.

There is **no** leader-election step — the plugin does not use one. The
comments in `kubernetes/deployment.yaml` say why.

---

## Test procedures

Each phase below is independent. Substitute `meta-minio` for `config-store` to run
the same procedure against MinIO; both are verified.

### Phase 1 — capture

```bash
kubectl apply -f - <<'EOF'
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: pg-source}
spec:
  instances: 1
  storage: {size: 1Gi}
  imageName: ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie
  imagePullPolicy: IfNotPresent
  postgresql:
    parameters: {shared_buffers: 128MB, work_mem: 16MB}
    pg_hba: ["host all all 10.244.0.0/16 scram-sha-256"]
  resources:
    requests: {cpu: 100m, memory: 256Mi}
  plugins:
    - name: chronicle.sharifmshaker.github.io
      parameters: {saveToStore: config-store}
  affinity:
    nodeSelector: {postgres.node.kubernetes.io: ""}
    tolerations: [{key: node-role.kubernetes.io/postgres, operator: Exists, effect: NoSchedule}]
EOF
```

| # | Do | Expect |
|---|---|---|
| 1.1 | wait for the cluster | one snapshot object under `<server>/chronicle/snapshots/` |
| 1.2 | change a GUC | a second snapshot object; generation advances, new `checksum` |
| 1.3 | `kubectl label cluster pg-source tier=gold` | a third snapshot with **generation unchanged** — labels do not bump `metadata.generation`, so the metadata fingerprint is what catches it |
| 1.4 | change a field no group captures, e.g. `enableSuperuserAccess` | generation advances, **no new object** — the extracted checksum matches what the store already holds |
| 1.5 | leave it idle 2 minutes | `resourceVersion` and log line count static — no requeue storm |

```bash
# 1.2
kubectl patch cluster pg-source --type=merge \
  -p '{"spec":{"postgresql":{"parameters":{"shared_buffers":"128MB","work_mem":"32MB"}}}}'

# watch what has actually been captured
./hack/dev/history.sh pg-source

# and the bucket
docker run --rm --network kind --entrypoint sh quay.io/minio/mc:latest -c \
  "mc alias set s http://objectstore-eu:9000 cnpg Cl0udNativePGRocks >/dev/null &&
   mc ls --recursive s/backups/pg-source/"
```

`shared_buffers` must not exceed `resources.requests.memory`, or CNPG's own
validating webhook rejects the patch — a real constraint, not a plugin problem.

Also confirm the snapshot never contains archiving configuration:

```bash
docker run --rm --network kind --entrypoint sh quay.io/minio/mc:latest -c \
  "mc alias set s http://objectstore-eu:9000 cnpg Cl0udNativePGRocks >/dev/null &&
   mc cat s/backups/pg-source/chronicle/latest.json" \
  | python3 -c "import sys,json; s=json.load(sys.stdin)['spec']
print('LEAK' if any(k in s for k in ('plugins','backup','externalClusters','bootstrap')) else 'clean')"
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
EOF
```

| # | Do | Expect |
|---|---|---|
| 2.1 | `kubectl get restorepolicy` | `Ready: True` — a statement about the rules; the policy reads no store |
| 2.2 | create a cluster with `restoreFrom`, asking for **more** storage than the source | persisted spec shows the source's smaller size — an update could never shrink it |
| 2.3 | check `metadata.annotations` | one key `chronicle.sharifmshaker.github.io/restored-from`, not nested objects |
| 2.4 | inspect the bucket | the restored cluster writes under **its own** `serverName` prefix |
| 2.5 | set `restoreFrom` to a name that does not exist | `kubectl apply` **denied**, message names the missing policy |
| 2.6 | set `saveTo` and `serverName` equal to the source | denied: would interleave the source's history |
| 2.7 | delete the `MutatingWebhookConfiguration`, then create a restoring cluster | cluster refused, `PhaseFailurePlugin`, no pods created |

```bash
# 2.3
kubectl get cluster pg-restored -o jsonpath='{.metadata.annotations}' | python3 -m json.tool
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

To test 3.3 without running a real backup, create a `Backup` and set its status
directly:

```bash
kubectl apply -f - <<'EOF'
apiVersion: postgresql.cnpg.io/v1
kind: Backup
metadata: {name: nightly-eu}
spec:
  cluster: {name: pg-source}
  method: barmanObjectStore
EOF

kubectl patch backup nightly-eu --subresource=status --type=merge \
  -p '{"status":{"backupId":"20260826T025730","stoppedAt":"2026-08-26T02:57:30Z","phase":"completed"}}'
```

```bash
kubectl get cluster pg-aligned \
  -o jsonpath='{.metadata.annotations.chronicle\.sharifmshaker\.github\.io/restored-from}' \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['alignedWith']); print(d['snapshotKey'])"
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
    - path: spec.postgresql.parameters.work_mem
      expression: 'pgMemFormat(pgMem(old) / 2, "kB")'
EOF

kubectl get restorepolicy scaled -o jsonpath='{.status.conditions}' | python3 -m json.tool
```

| # | Do | Expect |
|---|---|---|
| 4.1 | read `status.conditions` | `Ready: True` — paths resolve against the allowlist and CloudNativePG's types |
| 4.2 | restore a cluster | values scaled, not copied |
| 4.3 | mistype a transform path | `Ready: False` naming the field that does not exist; cluster creation denied |
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
| idle 2 minutes: no new log lines, `resourceVersion` static | CloudNativePG polls the plugin's status every 5s; a settled cluster must answer without provoking a write |
| idle 2 minutes: no object-store requests | the settled case answers from the watermark on the Cluster, without reading the bucket |
| restart the plugin, confirm no re-capture | the watermark lives in the bucket, not in the process |

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| every controller times out on `https://10.96.0.1:443/api` | `kube-proxy` crash-looping on inotify limits | raise `fs.inotify.max_user_instances` (see above) and delete the `kube-proxy` pods |
| `ImagePullBackOff` from ghcr.io | intermittent registry access | retry `docker pull` in a loop; it usually succeeds within ~4 attempts, then side-load |
| `kind load` succeeds but the image is absent | colima's containerd image store | `docker save … \| docker exec -i … ctr images import -` |
| image build takes 11+ minutes every time | BuildKit missing, cache mounts ignored | install `docker-buildx` and register `cliPluginsExtraDirs` |
| `mapfile: command not found` | macOS bash 3.2 | pass regions explicitly: `./scripts/setup.sh eu` |
| `Missing command kubectl-cnpg` / `cmctl` | `demo/setup.sh` prerequisites | `brew install kubectl-cnpg cmctl` |
| `Memory request is lower than shared_buffers` | CNPG cross-field validation | raise `resources.requests.memory` or lower `shared_buffers` |
| CNPG operator restarting repeatedly | CPU starvation on a small VM | delete unused test clusters; do not build images while testing |
| cluster stuck in `PhaseUnknownPlugin` | plugin Service not discovered | it must be in the operator's namespace with the `cnpg.io/pluginName` label and all three `cnpg.io/plugin*` annotations |
| `mapping values are not allowed in this context` applying a policy | unquoted CEL ternary | quote the expression |

Useful one-liners:

```bash
# plugin messages without the stack traces
kubectl -n cnpg-system logs -l app=chronicle --tail=50 | grep -oE '"(msg|error)":"[^"]{0,200}"'

# why a cluster is unhappy
kubectl get cluster pg-source -o jsonpath='{.status.phaseReason}'

# what the plugin has captured, read straight from the store
./hack/dev/history.sh pg-source
```

---

## Teardown

```bash
kubectl delete cluster --all
kubectl delete restorepolicy,configstore --all
make undeploy

cd cnpg-playground && ./scripts/teardown.sh eu

docker rm -f chronicle-minio
docker buildx prune          # only if you want the ~11 minute cold build back
```
