# The development environment

One command builds a complete, disposable environment: a kind cluster running
CloudNativePG, the barman-cloud plugin, an S3-compatible object store, this
plugin built from your working tree, and a PostgreSQL cluster wired to all of
it.

```bash
make dev-up          # or: ./hack/dev/up.sh
```

Roughly four minutes cold, under one warm. It finishes by printing the exact
commands to inspect what landed in the bucket.

```bash
make dev-reload      # rebuild the plugin and restart it, ~30s
make dev-history     # the captured configuration history
make dev-snapshot    # the newest snapshot document
make dev-down        # delete the cluster
```

Everything is provisioned by scripts in `hack/dev/`.

---

## Prerequisites

| Tool | Required | Why |
|---|---|---|
| `docker` | yes | runs the kind node and builds the image |
| `kind` | yes | the cluster |
| `kubectl` | yes | everything |
| `rclone` | no | shorter bucket-inspection commands; falls back to an in-cluster pod |
| `jq` | no | only `hack/dev/history.sh` needs it |

```bash
brew install docker kind kubectl rclone jq
```

### Raise the inotify limits first

The single most common cause of a mysteriously broken kind cluster. When
`fs.inotify.max_user_instances` is too low, kube-proxy crash-loops and **every
ClusterIP in the cluster stops resolving** — a symptom that looks nothing like
its cause. `up.sh` warns if it detects this, but cannot fix it for you:

```bash
# colima
colima ssh -- sudo sysctl -w fs.inotify.max_user_instances=512
colima ssh -- sudo sysctl -w fs.inotify.max_user_watches=524288

# Docker Desktop — Settings > Resources, or via the VM
docker run --rm --privileged --pid=host alpine:3 \
  nsenter -t 1 -m -u -n -i sysctl -w fs.inotify.max_user_instances=512
```

Budget about 3 GiB of memory for the VM. The environment is one node precisely
to keep that number small, but it is not free, and running it beside other kind
clusters on an 8 GiB machine will make both unhappy.

The failure mode is not obvious. Under CPU starvation the CloudNativePG operator
loses its leader-election lease and restarts cleanly — the log shows an orderly
`"Shutdown signal received"` rather than a crash, so it reads like something
asked it to stop. If you see the operator restarting every few minutes with no
error, check what else is running:

```bash
kind get clusters
docker ps --format '{{.Names}}' | wc -l
docker run --rm alpine:3 free -m
```

---

## What `up.sh` does

Nine steps, in dependency order. Every one is idempotent, so re-running is both
safe and the fastest way to reconcile an environment that has drifted.

### 1. Preflight

Checks for `docker`, `kind` and `kubectl` and that the Docker daemon answers.
`rclone` is optional — the script notes which inspection commands it will print
depending on whether you have it.

Then two environment checks that exist because both failures are unpleasant to
diagnose from their symptoms:

- **Host ports.** kind publishes the object store's ports when it creates the
  node, so a conflict surfaces as a wall of Docker output from deep inside
  `kind create cluster`. The check names the offending container and how to free
  it. It is skipped when the cluster already exists, since then the container
  holding those ports is our own node.
- **inotify.** Reads `fs.inotify.max_user_instances` from inside the Docker VM
  and warns below 512.

### 2. Resolve the CloudNativePG version

CloudNativePG publishes each release's operator manifest under `releases/` on
the `main` branch. The script asks the GitHub API for that directory, takes the
newest stable version, and installs it — so the environment tracks main as it
moves rather than pinning to whatever was current when this was written.

If the API is unreachable or rate-limited it falls back to a known-good version
and says so. To pin deliberately:

```bash
CNPG_VERSION=1.30.0 ./hack/dev/up.sh
```

The manifests are a release's; the image need not be. `CNPG_IMAGE` replaces the
operator image behind them, which is how to run a build that has not been
released yet:

```bash
CNPG_IMAGE=ghcr.io/cloudnative-pg/cloudnative-pg-testing:main ./hack/dev/up.sh
```

Since CRDs and RBAC still come from the release manifest, this is only sound for
a build close to it.

`up.sh` moves `OPERATOR_IMAGE_NAME` along with the container image, and both are
needed. The operator does not run the instance manager itself — it injects the
image that variable names as each instance's `bootstrap-controller` init
container, and the instance manager binary is copied out of it. Override only
the Deployment and you get a controller on one version driving instances on
another: here that meant a `main` operator probing `/startupz` over HTTPS while
a 1.30.0 instance manager answered plain HTTP, so no instance ever passed its
startup probe and the cluster never reported healthy.

### 3. Create the kind cluster

From `hack/dev/kind.yaml`. **One node, deliberately** — everything here is
pinned to the control-plane, and adding workers buys nothing while reliably
costing an afternoon to a pod that cannot route to `10.96.0.1:443`.

The config maps two node ports to your host, which is what makes the object
store reachable from outside the cluster with no port-forward to keep alive:

| In-cluster | Host | What |
|---|---|---|
| nodePort 30900 | `localhost:19000` | S3 API |
| nodePort 30901 | `localhost:19001` | console |

If the cluster already exists it is reused, not recreated.

**This switches your active kubectl context** to `kind-chronicle-dev`, because
step 8 runs `make deploy`, which applies to whatever context is current. The
summary prints the context name so it is never a surprise. `down.sh` and
`reload.sh` pass `--context` explicitly and do not move it.

### 4. cert-manager

CNPG-I is mutual TLS and the certificates are issued by cert-manager rather
than baked into images, so both the barman plugin and chronicle need it.

The script waits for the webhook's *endpoint* to have an address, not just for
the Deployment to report Available. Until the endpoint exists, every
`Certificate` apply is rejected — and the two conditions are several seconds
apart.

### 5. CloudNativePG

The operator manifest resolved in step 2.

### 6. The barman-cloud plugin

Brings its own `ObjectStore` CRD and its cert-manager `Certificate`s. It is a
real dependency of the default setup, because the environment's `ConfigStore`
is *derived* from a barman `ObjectStore` — the mode most people will use, since
it means the endpoint and credentials are written down once.

Scenario 05 shows the standalone mode, which needs neither the plugin nor
the CRD.

### 7. The object store

A single pod, a `Service` of type NodePort, a `Secret` holding credentials, a
`Job` that creates the bucket, and a long-lived `rclone` pod for inspecting the
store from inside the cluster.

**The endpoint is fully qualified**, and it has to be. The store lives in
`default` beside the clusters, but the plugin runs in `cnpg-system` — so the S3
client is in a different namespace from the Service, and a bare `objectstore`
name does not resolve from there. The symptom is misleading:

```
dial tcp: lookup objectstore on 10.96.0.10:53: no such host
```

while that exact name resolves fine from any pod in `default`. Hence
`http://objectstore.default.svc.cluster.local:9000`. If you write your own
`ConfigStore` or barman `ObjectStore` against a Service in another namespace,
qualify it the same way.

**RustFS by default.** See [The object store](#the-object-store) below.

The bucket-creation Job has `backoffLimit: 30` and is deleted-then-reapplied on
every run: Jobs are immutable once created, so re-applying an existing one
fails, and letting it retry is more honest than sleeping and hoping the store is
up.

### 8. Build and deploy the plugin

Builds the image from your working tree, loads it onto the node, applies
`kubernetes/`, and restarts the Deployment so a rebuilt image of the same tag is
actually picked up rather than silently ignored.

`kind load docker-image` can **report success and do nothing** when Docker is
backed by a containerd image store, which is the default under colima. The
failure is silent and shows up as an `ImagePullBackOff` for an image you can see
in `docker images`. So the load is verified against `crictl images` on the node,
and falls back to streaming the image in:

```bash
docker save "$IMG" | docker exec -i "$NODE" ctr --namespace k8s.io images import -
```

### 9. The stores and the cluster

Applies the barman `ObjectStore`, the chronicle `ConfigStore` derived from it,
and a one-instance PostgreSQL cluster using both plugins. Then waits for three
things in order: the store to report `Ready`, the cluster to report healthy, and
the first configuration snapshot to appear in the bucket.

That last wait is what makes a successful run mean something. The script does
not claim the environment is up until chronicle has actually captured a
snapshot.

---

## What you end up with

```
kind cluster  chronicle-dev            context  kind-chronicle-dev

namespace cnpg-system
  cnpg-controller-manager              the operator, from main
  barman-cloud                         WAL archiving and base backups
  chronicle                            this plugin, from your working tree

namespace default
  objectstore                          RustFS + a NodePort service
  rclone                               toolbox pod for inspecting the bucket
  ObjectStore/backup-store           barman's view of the store
  ConfigStore/config-store                chronicle's view, derived from it
  Cluster/pg-source                        one instance, both plugins
```

One bucket, `s3://chronicle/`, holding both kinds of artifact:

```
chronicle/
└── pg-source/
    ├── wals/                              barman WAL segments
    │   └── 0000000100000000/
    │       └── 000000010000000000000001.gz
    ├── base/                              barman base backups — appears once a
    │                                      backup is taken; see scenario 04
    └── chronicle/
        ├── latest.json                    pointer to the newest snapshot,
        │                                  so a read does not need a LIST
        └── snapshots/
            └── 20260827T030907Z-g0000000001-229feffe.json
```

That is real output from `rclone tree dev:chronicle` on a freshly provisioned
environment, one generation in.

That layout is the point of the project. `chronicle/` is a sibling of barman's
directories, which barman never enumerates — so configuration travels with the
data as one self-contained artifact. When you restore from a backup you may have
lost the cluster, the namespace, and access to the git repo; you still have the
bucket.

Snapshot keys are content-addressed and prefixed with a UTC timestamp, so
lexical order is chronological.

---

## Inspecting the bucket

With host `rclone`, point it at the store once per shell:

```bash
eval $(./hack/dev/env.sh)

rclone tree dev:chronicle                              # everything
rclone ls   dev:chronicle/pg-source/chronicle/snapshots/   # configuration snapshots
rclone ls   dev:chronicle/pg-source/wals/                  # barman WAL
rclone ls   dev:chronicle/pg-source/base/                  # barman base backups (after scenario 04)
rclone cat  dev:chronicle/pg-source/chronicle/snapshots/<key> | jq
```

`env.sh` prints `RCLONE_CONFIG_DEV_*` exports rather than writing a config file,
so nothing lands in `~/.config/rclone` and the settings die with the shell.

Without host rclone, the same thing through the in-cluster pod:

```bash
kubectl exec deploy/rclone -- rclone tree dev:chronicle
```

Or in a browser at <http://localhost:19001> — user `chronicle`, password
`chronicle123`.

Two helpers wrap the common cases and work either way:

```bash
./hack/dev/snapshot.sh              # the newest snapshot, as JSON
./hack/dev/history.sh               # the whole history, as a table
./hack/dev/history.sh pg-source shared_buffers max_connections
```

```
CAPTURED (UTC)           GEN  STORAGE     shared_buffers/max_connections
----------------------  ----  ----------  ------------------------------
2026-08-26T20:07:08        1  2Gi         96MB  120
2026-08-26T20:31:44        2  2Gi         256MB  120
```

---

## Scenarios

`hack/dev/scenarios/` holds six manifests that each demonstrate one behaviour
against the running environment. Every one is a single `kubectl apply`, and each
file's header comment says what to run afterwards and what to expect.

| File | Shows |
|---|---|
| `01-restore-basic.yaml` | restoring configuration into a new cluster, with skips |
| `02-restore-transform.yaml` | rescaling values with CEL, and the path checks that catch typos first |
| `03-restore-at-time.yaml` | the configuration in force at a given moment |
| `04-restore-pitr.yaml` | deriving that moment from the cluster's own recovery target |
| `05-standalone-store.yaml` | a store configured directly, with no barman dependency |
| `06-guardrails.yaml` | five things that are refused — two policies by their controller, three clusters at admission — and why |

See `hack/dev/scenarios/README.md` for a suggested order.

---

## Configuration

Every knob lives in `hack/dev/config.sh` and is overridable from the
environment:

| Variable | Default | Notes |
|---|---|---|
| `KIND_CLUSTER` | `chronicle-dev` | kubectl context is `kind-` + this |
| `CNPG_VERSION` | newest on main | pin to override |
| `CNPG_IMAGE` | the release's own | run a different operator build behind the release manifests |
| `CERT_MANAGER_VERSION` | `v1.21.1` | |
| `BARMAN_PLUGIN_VERSION` | `v0.14.0` | |
| `DEV_S3` | `rustfs` | or `seaweedfs` |
| `DEV_BUCKET` | `chronicle` | |
| `DEV_ACCESS_KEY` / `DEV_SECRET_KEY` | `chronicle` / `chronicle123` | dev credentials, deliberately trivial |
| `DEV_S3_HOST_PORT` | `19000` | must match `kind.yaml` |
| `DEV_CONSOLE_HOST_PORT` | `19001` | must match `kind.yaml` |
| `DEV_CLUSTER_NAME` | `pg-source` | |
| `IMG` | `ghcr.io/…/cnpg-i-chronicle:dev` | |

```bash
DEV_S3=seaweedfs ./hack/dev/up.sh
CNPG_VERSION=1.29.1 KIND_CLUSTER=chronicle-129 ./hack/dev/up.sh
```

Changing the host ports means editing `hack/dev/kind.yaml` too, and recreating
the cluster — `extraPortMappings` is fixed when the node is created.

The manifests in `hack/dev/manifests/` use `${VAR}` placeholders substituted by
a small `sed` in `hack/dev/lib.sh` rather than by `envsubst`, which is not
installed on macOS by default.

---

## The object store

The plugin works with any S3-compatible endpoint. This environment needs one
that runs as a single pod inside kind.

**RustFS** is the default because cnpg-playground uses it, which means
barman-cloud writing WAL and base backups into RustFS is exercised by the
CloudNativePG project itself — the same workload this environment runs. It is
Apache-2.0 and actively developed. The honest caveat is that it has not cut a
stable 1.0, so the image is pinned rather than tracking `latest`.

**SeaweedFS** is the hedge, one variable away (`DEV_S3=seaweedfs`). It is
Apache-2.0 and cuts stable releases.

**rclone** is the inspection tool. It is MIT, vendor-neutral, and speaks to any
S3 endpoint.

---

## Troubleshooting

**Everything is `Pending` or the operator crash-loops.** Almost always memory.
Check `docker run --rm alpine:3 free -m`; give the VM more, or delete other kind
clusters (`kind get clusters`).

**A pod is in `ImagePullBackOff` for an image you can see locally.** `kind load`
silently no-opped. `up.sh` handles this, but if you loaded by hand:

```bash
docker save ghcr.io/sharifmshaker/cnpg-i-chronicle:dev \
  | docker exec -i chronicle-dev-control-plane ctr --namespace k8s.io images import -
```

**Every ClusterIP fails to connect.** kube-proxy is crash-looping on inotify
limits. See [above](#raise-the-inotify-limits-first).

```bash
kubectl -n kube-system get pods -l k8s-app=kube-proxy
```

**`lookup <service> ... no such host` in the plugin log.** The endpoint is not
namespace-qualified. The plugin runs in `cnpg-system`, so a bare Service name
from another namespace does not resolve — use
`<service>.<namespace>.svc.cluster.local`.

**A Cluster is stuck in `PhaseUnknownPlugin`.** The plugin's Service is not
reachable, or it is not in the operator's namespace — CloudNativePG's
`isPluginService` rejects Services elsewhere, and does so silently.

```bash
kubectl -n cnpg-system get svc chronicle
kubectl -n cnpg-system logs deploy/chronicle
```

**`up.sh` times out waiting for a snapshot.** The cluster came up but capture
did not happen. Check the store resolved and the plugin is reachable:

```bash
kubectl get configstore config-store -o yaml
kubectl -n cnpg-system logs deploy/chronicle --tail=50
```

**A Cluster is stuck `unrecoverable` with no instance, right after you deleted
and recreated it under the same name.** Its WAL archive outlived it. CloudNativePG
found a non-empty archive for that server name, concluded the cluster was already
bootstrapped, and declined to initialise it — so there is no PVC and no pod.

```bash
kubectl delete cluster pg-source
eval $(./hack/dev/env.sh) && rclone purge dev:chronicle/pg-source
./hack/dev/up.sh
```

Simpler, and what to reach for unless you have a reason not to:

```bash
make dev-down && make dev-up
```

`down.sh --keep` avoids the whole situation by never deleting the environment's
own source cluster.

**A cluster is healthy but no new snapshots appear.** Compare what the plugin
reports against what is actually in the bucket:

```bash
eval $(./hack/dev/env.sh) && rclone ls dev:chronicle/pg-source/chronicle/snapshots/
./hack/dev/history.sh pg-source
```

The store is the only record, so there is nothing that can disagree with it.
If snapshots are removed out of band, capture resumes at the cluster's next
change: while a Cluster sits on the generation and metadata its published
watermark records, neither the capture hook nor the status hook reads the
bucket. `kubectl annotate cluster pg-source chronicle-poke=1` is enough to move
it.

The plugin also publishes this on the Cluster:

```bash
kubectl get cluster pg-source \
  -o jsonpath='{.status.pluginStatus[?(@.name=="chronicle.sharifmshaker.github.io")].status}'
```

**Start over.** `make dev-down && make dev-up`. Deleting the cluster takes the
object store's volume with it, so the next run starts from an empty bucket
rather than one holding snapshots written by a previous shape of the code —
which is usually what you want when something is behaving strangely.

---

## Relationship to `docs/TESTING.md`

`TESTING.md` is the *manual* procedure: how to assemble this environment by
hand, and what to do and expect at each phase of the plugin. It is the
reference when you need to understand a step or check a behaviour
deliberately.

This environment automates the common path. Use `up.sh` day to day; reach for
`TESTING.md` when you need to vary something it does not expose.
