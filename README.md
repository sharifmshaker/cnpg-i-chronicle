# cnpg-i-chronicle

A [CloudNativePG](https://cloudnative-pg.io) plugin that versions a Cluster's
**configuration** to an object store, and restores it when a cluster is created.

CloudNativePG backs up data. Nothing versions the spec. When you restore from a
backup you get the bytes back and then rebuild the configuration — GUCs, CPU and
memory, storage sizing, affinity, topology spread — from whatever manifest you
hope is current. chronicle closes that gap so a restored cluster is a restore of
the data *and* the configuration that was running when that data was written.

It piggybacks on the bucket you already use for backups: snapshots go to
`<destinationPath>/<serverName>/chronicle/`, a sibling of barman-cloud's `base/`
and `wals/`.

## Status

**Alpha.** All four phases work and are verified end to end against a real
CloudNativePG operator with both RustFS and MinIO, but this has not been run in
production by anyone. It is maintained on a best-effort basis; there is no
support commitment.

| Phase | State |
|---|---|
| 1. Capture | Implemented |
| 2. Restore | Implemented |
| 3. Alignment with a backup's recovery target | Implemented |
| 4. CEL transforms | Implemented |

`select.mode` offers `Latest`, `Generation`, `Timestamp` and
`AlignWithRecoveryTarget`.

Only `s3://` destinations are implemented, but that covers **any S3-compatible
endpoint** — AWS S3, MinIO, RustFS, Ceph — because a custom `endpointURL`
switches the client to path-style addressing, which self-hosted servers require.
Azure and GCS return an explicit error rather than silently doing nothing.

## How it works

**Capture** runs in `ReconcilerHooks.Post` for `KIND_CLUSTER`, at the end of a
successful reconciliation when the spec has settled. It compares
`metadata.generation` and a fingerprint of the captured metadata against what it
last saw, and writes a snapshot only when those move.

The object store is the source of truth. When the in-process record misses, the
hook reads `latest.json` and compares checksums before writing, so a generation
bump that changed nothing captured produces no second object — and a restart
re-reads the store rather than re-capturing.

That watermark is also published on the Cluster itself, so
`kubectl get cluster -o json` answers "is my configuration being captured?"
without reaching for the bucket — and the hook reads it straight out of the
Cluster it was handed, so the settled case costs no reads at all.

Generation alone is not enough: Kubernetes does not bump `metadata.generation`
for label or annotation edits, so the hook also compares a cheap fingerprint of
the captured labels and annotations. Without it a `kubectl label` would go
unrecorded until some unrelated spec change happened to flush it out.

The fingerprint also covers the capture rules: each group's paths, the denylist,
the metadata filter, and CloudNativePG's own lists of fixed parameters and
managed libraries. A plugin upgrade or dependency bump that captures more, or
differently, therefore writes one new snapshot per cluster recording the new
rules, even when the content comes out the same, instead of waiting for the
next unrelated edit.

The bucket is created on first write if it does not exist, matching
barman-cloud, since neither MinIO nor RustFS creates one implicitly. If
`s3:CreateBucket` is denied the error says so and names the bucket.

**Restore** runs in the plugin's own mutating admission webhook at `CREATE`,
scoped by a CEL `matchConditions` expression matching on the plugin being
configured. It is not CNPG-I's `Operator.MutateCluster`: that RPC exists in the
protocol but has had no caller in the operator since remote plugin support
replaced the unix-socket loader the admission webhook used to build
(`a0648d4ba`, "feat(cnpg-i): support remote plugins"), and the upstream request
to reinstate it was closed `not_planned`.

Running at `CREATE` is not just a workaround for that. It is what makes a
faithful restore possible: on update CloudNativePG refuses to shrink storage,
refuses to change `postgresUID`/`postgresGID`, and rejects an image change and a
GUC change in the same request. At creation none of those apply, so a snapshot
taken from a 1Gi cluster restores cleanly over a manifest that asked for 5Gi.

The merge is at **leaf granularity**, which is what makes
`skip: [{path: spec.postgresql.parameters.work_mem}]` meaningful — replacing the
parameters map wholesale could not carve out one GUC. It is additive and never
deletes: a field the target sets and the snapshot does not know about survives.

Everything runs operator-side. Unlike barman-cloud, nothing is injected into
instance pods: no sidecar image, no per-cluster RBAC.

## What is captured

Capture is an **allowlist** of named groups, so a field CloudNativePG adds in a
future release is not carried between clusters until someone adds it
deliberately. Default groups: `Gucs`, `Resources`, `Storage`, `Scheduling`,
`Instances`, `Metadata`, `Image`, `Monitoring`, `Managed`, `PodEnv`,
`Replication`, `Timings`, `Security`, `Misc`. Opt-in: `Identity`, `Secrets`.

`Gucs` covers the whole `.spec.postgresql` stanza — `parameters`, `pg_hba`,
`pg_ident`, `shared_preload_libraries`, `synchronous` — and also
`.spec.podSelectorRefs`, which lives outside it. That last one is deliberate: a
`pg_hba` rule can address a client set as `${podselector:NAME}`, and restoring
the rules without the selectors they name would leave host-based authentication
pointing at nothing.

The denylist is re-checked when restoring, not trusted from capture time: a
snapshot is an input read out of a bucket, and it may have been written by an
older build or edited by hand.

Three paths are on a hard denylist that no configuration can re-enable:
`.spec.plugins`, `.spec.backup` and `.spec.externalClusters`. Those describe
*where a cluster archives its WAL*; copying them into a restored cluster would
point two live clusters at the same server name in the same bucket. Also denied:
`.spec.bootstrap`, `.spec.replica`, object identity, and all of `.status`.

PostgreSQL parameters in `postgres.FixedConfigurationParameters` are dropped —
the list is imported from the operator, not copied, so a new fixed parameter
upstream is handled by a dependency bump. Operator-managed
`shared_preload_libraries` entries are stripped too.

Labels and annotations are captured except for keys that are wrong to carry to
any cluster: those under `cnpg.io` and its subdomains, this plugin's own, and
`kubectl.kubernetes.io/last-applied-configuration`. Keys belonging to other
tools, such as Argo CD, Helm or Flux, are captured like any other, because
whether they should travel depends on how the target is managed. A
`RestorePolicy` decides that; see
[Choosing which labels and annotations travel](#choosing-which-labels-and-annotations-travel).

## Restoring

The simplest restore needs no policy at all — just the history to read:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pg-restored
spec:
  instances: 1
  storage:
    size: 5Gi
  plugins:
    - name: chronicle.sharifmshaker.github.io
      parameters:
        restoreFromStore: config-store   # the ConfigStore holding the history
        restoreFromServer: pg-prod       # whose history, within that store
```

A `RestorePolicy` adds rules — which snapshot, what to leave alone, what to
rewrite. It carries **no source of its own**, so one policy applies to any
number of restores from any number of stores:

```yaml
apiVersion: chronicle.sharifmshaker.github.io/v1
kind: RestorePolicy
metadata:
  name: dev-sizing
spec:
  select:
    mode: Latest           # | Generation | Timestamp | AlignWithRecoveryTarget
  skip:
    - group: Resources                                # keep local sizing
    - path: spec.postgresql.parameters.work_mem       # keep one GUC
  transform:
    - path: spec.storage.size
      expression: 'old.divide(10)'
```

```yaml
  plugins:
    - name: chronicle.sharifmshaker.github.io
      parameters:
        restoreFromStore: config-store
        restoreFromServer: pg-prod
        restorePolicy: dev-sizing        # the same policy, any source
```

Unset `select.mode` follows the Cluster: **`AlignWithRecoveryTarget` when it
bootstraps from a recovery**, `Latest` otherwise. Those two cases want opposite
things — a cluster recovering data to last Tuesday wants last Tuesday's
configuration — and getting it wrong is silent, so it is not left to be
configured.

Check a policy is usable before you rely on it:

```console
$ kubectl get restorepolicy
NAME         MODE     READY   AGE
dev-sizing   Latest   True    2m
```

`Ready` is a statement about the rules, not about any history: a policy names no
source, and which snapshot it is applied to is decided by the Cluster being
created. It covers rule syntax, expression compilation, paths that contradict
each other, and whether each path could ever be captured at all — one on the
denylist, one no capture group claims, or one misspelled against
CloudNativePG's own field names. The webhook runs the same checks again at
restore time, so a policy that is `Ready: False` is refused with the same
message even if its status has not caught up. What has to wait for a real
snapshot is only whether a path that is valid in principle is present in that
particular history, and what an expression produces from a real value.

### Choosing which labels and annotations travel

A skip rule whose path names a map, such as `metadata.labels`,
`metadata.annotations` or `spec.postgresql.parameters`, can narrow itself to
some keys. `keyPrefix` matches keys starting with a string, and `except` leaves
named keys alone. Keys are matched whole, dots included.

```yaml
  skip:
    - path: metadata.labels                  # no labels at all
    - path: metadata.annotations             # only these annotations
      except: [example.com/owner]
```

A cluster managed by a GitOps tool should usually not inherit the source's
ownership markers. Otherwise the tool can treat the restored cluster as part of
the source's application, and prune it:

```yaml
  skip:
    - path: metadata.labels
      keyPrefix: app.kubernetes.io/          # Argo CD's default tracking label, Helm
    - path: metadata.annotations
      keyPrefix: argocd.argoproj.io/
    - path: metadata.annotations
      keyPrefix: meta.helm.sh/
    - path: metadata.labels
      keyPrefix: kustomize.toolkit.fluxcd.io/
```

Each rule removes a set of keys, and `except` exempts keys from its own rule
only. A path naming one key inside a map skips exactly that key:
`metadata.annotations.example` leaves `example.com/owner` alone.

### Matching the configuration to a point-in-time restore

`select.mode: AlignWithRecoveryTarget` reads the Cluster's own
`bootstrap.recovery` stanza and picks the configuration that was in force at the
moment the recovered data corresponds to. Restoring data to last Tuesday and
then applying today's configuration produces a cluster that never existed; this
keeps the two in step without a timestamp being copied by hand.

| What the Cluster sets | How the moment is found |
|---|---|
| `recoveryTarget.targetTime` | used directly |
| `recoveryTarget.backupID` | the `Backup` resource that recorded it, via `.status.stoppedAt` |
| `bootstrap.recovery.backup.name` | that `Backup`'s `.status.stoppedAt` |
| a recovery with no target | the newest configuration — recovery replays the whole archive |
| `targetXID` / `targetName` / `targetImmediate` | anchored to the mandatory `backupID`; **approximate**, since WAL replays past the backup |
| `targetLSN` with no `backupID` | unresolvable |

`targetTime` is parsed with the same helper CloudNativePG's own webhook uses, so
a value it accepts is interpreted identically here.

When the target cannot be mapped to a moment, `onUnresolvable` decides:
`Fail` (default) refuses the cluster, `UseLatest` accepts the newest
configuration. Fail is the default because the alternative is a cluster whose
data and configuration silently disagree. Either way the restore record says
what happened:

```json
{"alignedWith": "recoveryTarget.targetXID anchored to backupID 20260826T025730;
  the configuration is taken from the end of that backup, which is the closest
  knowable point (2026-08-26T02:57:30Z)"}
```

### Rescaling values on the way in

A `transform` rewrites a captured value with a CEL expression — the same
language Kubernetes uses for CRD validation rules — so a cluster can inherit
production's shape without inheriting its size.

```yaml
  transform:
    - path: spec.storage.size
      expression: 'old.divide(10)'
    - path: spec.instances
      expression: 'old > 1 ? 1 : old'
    - path: spec.postgresql.parameters.shared_buffers
      expression: 'pgScale(old, 0.5)'
```

What `old` is depends on the path, following how CloudNativePG types the field:

| Path | `old` is | Example |
|---|---|---|
| a resource quantity: `spec.resources.*`, `spec.storage.size`, `spec.walStorage.size`, `spec.ephemeralVolumesSizeLimit.*` | quantity, even when written `2` | `old.multiply(5)`, `old.divide(2)`, `old.add(quantity('1Gi'))` |
| an integer field such as `spec.instances` | integer | `old * 2`, `old > 3 ? 3 : old` |
| a string holding a plain number, such as `max_connections: "200"` or `random_page_cost: "1.1"` | integer or double | `old * 2`, `old * 2.0` |
| any other string, such as `shared_buffers: "128MB"` | string | `pgScale(old, 2)` |

Deciding by path keeps one policy consistent across histories: a CPU request of
`2` and one of `500m` are both quantities, so `old.multiply(2)` works on either.
The result is written back in the captured value's own type, so a transformed
GUC stays a string. CEL does not mix integers and doubles implicitly, so scaling
an integer by a fraction is `double(old) * 0.5`.

Sizes use member calls rather than `old * 5`: cel-go binds arithmetic operators
as singletons in its standard library and rejects specialized overloads on them,
so a custom quantity type cannot join in. Plain integers take the operators
directly. Scaling preserves format, so `10Gi` times five is `50Gi`, not
`53687091200`.

PostgreSQL memory settings are deliberately **not** treated as Kubernetes
quantities. PostgreSQL's `MB` is binary while Kubernetes' `M` is decimal, and
conflating them would shift every restored memory setting by about 5%. `pgScale`
and `pgMem`/`pgMemFormat` handle those and keep the source's unit.

Quote every expression in YAML — a ternary contains a colon, and unquoted
`old > 1 ? 1 : old` parses as a mapping.

Expressions compile when the `RestorePolicy` is applied, which catches syntax
errors and unknown functions, and every path is checked against the capture
allowlist and CloudNativePG's own types — so `spec.storage.sizze` is refused on
the policy rather than on a cluster. Because `old` is dynamically typed, a
mismatch — doubling a GUC whose value is `on` — can only surface on evaluation,
against a real value.

A transform whose path matches nothing, or that targets a skipped path, fails
the restore rather than silently doing nothing — an expression meant to shrink a
cluster that never ran would leave it at production's size. A transform aimed at
an object rather than a value fails for the same reason: the values beneath it
would restore untransformed.

Set `onMissing: Ignore` on a rule whose path some histories legitimately lack —
capture groups gain paths as CloudNativePG gains fields, and snapshots outlive
that, so a transform that is correct today can name something a snapshot from
last year never carried. It covers absence only; a skipped path or an object
path still fails.

Two things are refused at `kubectl apply`, with the reason on the terminal
rather than in a log somewhere:

- a `restoreFrom` naming a policy or store that does not exist, or a `select`
  that matches no snapshot;
- a cluster whose `saveTo` resolves to the same store **and** server name it is
  restoring from, which would interleave its own history with the source's.

If the webhook did not run at all — the plugin was installed after the cluster,
or its `MutatingWebhookConfiguration` was removed — the Pre-reconcile guard
refuses the cluster instead of letting it come up with none of its configuration
restored.

## Usage

```yaml
apiVersion: chronicle.sharifmshaker.github.io/v1
kind: ConfigStore
metadata:
  name: config-store
spec:
  derivedFrom:
    name: backup-store       # an existing barmancloud.cnpg.io/v1 ObjectStore
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pg-source
spec:
  instances: 3
  storage:
    size: 10Gi
  plugins:
    - name: barman-cloud.cloudnative-pg.io
      isWALArchiver: true
      parameters:
        barmanObjectName: backup-store
    - name: chronicle.sharifmshaker.github.io
      parameters:
        saveToStore: config-store
```

`derivedFrom` reuses a barman-cloud `ObjectStore`. A `ConfigStore` can equally
carry its own `configuration` block, so the plugin works with no backup plugin
installed at all. More in `hack/examples/`.

That block is scoped to what the plugin actually uses — `destinationPath`,
`endpointURL`, `endpointCA` and `s3Credentials` — rather than embedding
barman-cloud's configuration type. barman's is shaped for a backup tool and also
carries WAL and base-backup compression, encryption, parallelism, command
arguments and object tags; embedding it would put all of that in the CRD and in
`kubectl explain` as settings this plugin silently ignores. Deriving from a
barman `ObjectStore` projects onto the same four fields, so a store using Azure
or GCS is reported when it is resolved rather than when a snapshot is first
written. The credential field names are identical to barman's, so that block can
be copied across unchanged.

Progress is visible in two places:

```console
$ kubectl get configstore
NAME           DESTINATION     SOURCE                    RETENTION   READY   AGE
config-store   s3://backups/   ObjectStore/backup-store  Forever     True    5m

$ ./hack/dev/history.sh pg-source
CAPTURED (UTC)           GEN  STORAGE     shared_buffers/work_mem/max_connections
----------------------  ----  ----------  ----------------------------
2026-08-27T17:15:01        1  2Gi         96MB  12MB  120
```

A store's `READY` means capture can actually use it. The configuration
resolves, the credential Secrets can be read, and the object store accepts one
small listing under the destination path, a permission capture needs anyway.
A bucket that does not exist yet still counts as ready, since the first write
creates it. The check reruns every ten minutes, and every minute while the
store is not ready.

## Snapshot format

```
<destinationPath>/<serverName>/chronicle/
  snapshots/20260825T120000Z-g0000000042-3dab90cc.json   # time-g<gen>-<hash8>
  latest.json                                            # pointer, avoids a LIST
```

Keys sort lexicographically into chronological order. The trailing content hash
keeps two captures of the same generation inside the same second from colliding
— which happens in practice, because a label edit does not bump `generation` —
and makes a snapshot's key a pure function of its content, so `latest.json` can
name the object it copies without a second request.

```json
{
  "apiVersion": "chronicle.sharifmshaker.github.io/v1",
  "kind": "ClusterMetadataSnapshot",
  "capturedAt": "2026-08-25T12:00:00Z",
  "generation": 42,
  "cluster": {"name": "pg-source", "namespace": "default", "uid": "..."},
  "source": {"pluginVersion": "0.1.0"},
  "groups": ["Gucs", "Resources", "..."],
  "captureRules": "sha256:...",
  "spec": {"instances": 3, "storage": {"size": "10Gi"}},
  "checksum": "sha256:..."
}
```

The checksum covers only `spec`, `labels` and `annotations` — not `capturedAt`
or `generation`, which move on every capture. That is what lets a generation
bump touching nothing we capture skip the write.

## Retention

By default nothing is ever deleted. `spec.retention` on a `ConfigStore` sets
how far back a restore must still be able to reach:

```yaml
spec:
  retention: 30d                     # or 4w, 6m
  # retention: Forever               # the default
  # retention: InheritFromObjectStore  # follow barman's retentionPolicy
```

**This is a guarantee, not a deletion window**, and the difference matters.
Backups are periodic, so deleting everything older than 30 days still leaves a
month of them. Snapshots are written *only when configuration changes*, so a
cluster last edited a year ago has exactly one snapshot, a year old — and it
describes what is running right now. Deleting it for being old would leave every
restore in the window with nothing to select.

So `30d` means "any restore target within the last 30 days must resolve". What
that keeps:

- every snapshot from the last 30 days
- **plus the newest one before that** — the anchor, however old it is
- deleting only what the anchor supersedes, which no target in the window could
  ever have selected

A cluster that never changes keeps its single snapshot forever.

Targets *older* than the window are refused, not answered approximately.
Selection asks for the newest snapshot at or before the target, and once the
history no longer reaches that far there is nothing to return:

```
no snapshot captured at or before 2025-07-23T00:00:00Z; the earliest is
2025-10-31T00:00:00Z. The source cluster's configuration history does not
reach back that far
```

So retention narrows what you can restore, and says so, rather than quietly
pairing recovered data with a configuration that was never in force alongside
it. In practice it rarely bites: backup retention means there is no data to pair
an older configuration with anyway.

Enforcement runs in the ConfigStore's periodic sweep and needs
`s3:DeleteObject`. Without that grant, deletion is logged and skipped — the
store keeps capturing and restoring rather than going NotReady over disk usage.
`status.resolved.retention` reports the window actually being enforced.

Retention applies to histories that a live Cluster still archives into. A
deleted cluster's history is left alone, as is one belonging to a cluster that
has since repointed `saveToStore` elsewhere.

That is the same shape as barman-cloud, whose retention runs in the instance
sidecar on the current primary and prunes only that pod's own server name — so
deleting a Cluster leaves its base backups and WAL behind too. Neither plugin
reaps orphaned prefixes, and keeping them consistent is the point: pruning
configuration while the data survives would leave restorable backups with no
record of the configuration that ran alongside them.

## Requirements

CloudNativePG >= 1.28, cert-manager, and Kubernetes >= 1.28 — the restore
webhook scopes itself with `matchConditions`, which is beta-on-by-default from
1.28 and GA from 1.29.

> **Until the next CloudNativePG patch releases.** Every release up to and
> including 1.28.4, 1.29.2 and 1.30.0 clears a plugin's status on the Cluster
> about as fast as it is written. The fix,
> [cloudnative-pg#11386](https://github.com/cloudnative-pg/cloudnative-pg/pull/11386),
> is merged and backported but not yet released. Against those releases the
> plugin still works correctly, but the capture watermark it publishes rarely
> stays on the Cluster. The status hook then reads `latest.json` from the object
> store on each five-second poll, for every Cluster that sets `saveToStore`.
> The minimum version will move to the first releases carrying the fix.

The plugin must be deployed in **the operator's namespace**. CloudNativePG's
`isPluginService` rejects Services anywhere else, and does so silently: the
symptom is a Cluster stuck in `PhaseUnknownPlugin`, with nothing logged about
the Service.

### Permissions

The plugin runs under one `ClusterRole`. Everything it touches outside its own
API group is **read-only**:

| API group | Resources | Verbs | Why |
|---|---|---|---|
| `""` | `secrets` | get | Object-store credentials, which live in the Cluster's namespace |
| `postgresql.cnpg.io` | `clusters`, `backups` | get, list | Retention lists the Clusters archiving into a store; `backups` resolves a `backupID` to a point in time |
| `barmancloud.cnpg.io` | `objectstores` | get | Deriving a store's configuration from an existing backup target |
| `chronicle.…` | `configstores`, `restorepolicies` | get, list, watch | Its own resources |
| `chronicle.…` | `…/status` | get, update, patch | Ready conditions and the resolved store configuration |

Two things follow that are worth stating plainly:

- **It can read Secrets cluster-wide.** Credentials are referenced from the
  Cluster's namespace, which the plugin does not know ahead of time, so the role
  cannot be namespaced without one `RoleBinding` per namespace. If that trade is
  wrong for you, replace the `ClusterRoleBinding` in `kubernetes/rbac.yaml` with
  per-namespace `RoleBinding`s; nothing in the code assumes cluster scope.
- **It never writes to a Cluster.** It has no `clusters/status` permission and no
  `update` on `clusters`. Configuration reaches a Cluster only through the
  admission webhook, at creation, in the response to the API server — which is
  also why a restore cannot be retrofitted onto a cluster that already exists.
  The watermark in `.status.pluginStatus` is no exception: the plugin returns it
  over CNPG-I and CloudNativePG is what writes it.

## Limitations

Worth knowing before you adopt it.

**It restores tuning, not wiring.** `.spec.plugins`, `.spec.backup`,
`.spec.externalClusters` and `.spec.bootstrap` are never restored, and no
configuration can re-enable them. Copying them would point a second live cluster
at the source cluster's WAL archive. So this will not reconstruct a cluster's
backup configuration or its bootstrap stanza — you still write those yourself.

**Adopting it makes the plugin a dependency of every Cluster that lists it.**
This is inherent to CNPG-I, not specific to this plugin: a Cluster naming a
plugin the operator cannot reach stops reconciling and reports
`PhaseUnknownPlugin`. Capture-only clusters are affected too. What the restore
webhook's `matchConditions` do buy you is that *admission* risk is opt-in per
cluster — a cluster without `restoreFrom` never reaches the webhook, so a plugin
outage cannot block its creation.

**Restore is create-only.** It cannot be retrofitted onto an existing cluster,
by design: at CREATE none of CloudNativePG's update-immutability rules apply,
which is what lets a snapshot shrink storage or set `postgresUID`.

**Restoring writes values the target's manifest did not specify.** If you manage
clusters with Argo CD or Flux, every field injected at creation reads as drift
against git until you reconcile the two. Consider whether you want
`restoreFrom` on a GitOps-managed cluster at all, or only on ones created by
hand during a recovery.

**Only S3-compatible object stores.** Azure Blob and GCS are rejected with an
explicit error rather than silently ignored. `docs/EXTENDING.md` documents what
adding one takes.

**`targetLSN` with no `backupID` cannot be aligned.** There is nothing to anchor
it to without replaying WAL. It fails, unless `onUnresolvable: UseLatest`.

**Snapshots are kept forever by default.** They are ~3KB and written only when
configuration changes, so a cluster edited daily for a decade costs ~11MB.
Set `spec.retention` on the ConfigStore to bound it.

**Whether `.metadata.labels` reach pods depends on operator-level config.** The
`INHERITED_LABELS`/`INHERITED_ANNOTATIONS` settings live in the operator's
ConfigMap, outside the Cluster, and are not captured. `.spec.inheritedMetadata`
is self-contained and is.

**No metrics.**

## Development

```bash
make dev-up        # a complete environment in one command: kind + CNPG + barman
                   # + object store + this plugin + a cluster wired to all of it
```

Roughly four minutes cold. It finishes by printing the exact commands to inspect
the backups and configuration snapshots it wrote. Then:

```bash
make dev-reload    # rebuild the plugin and restart it, ~30s
make dev-history   # the captured configuration history, as a table
make dev-down      # delete it
```

Six runnable scenarios live in `hack/dev/scenarios/`, each a single
`kubectl apply` demonstrating one behaviour — restore, CEL transforms,
point-in-time alignment, and the guardrails that refuse a bad configuration.

[`docs/DEV-ENVIRONMENT.md`](docs/DEV-ENVIRONMENT.md) explains what the script
does at each step and why. [`docs/UPSTREAM.md`](docs/UPSTREAM.md) records the
CloudNativePG behaviour that shaped this plugin's design.
[`docs/TESTING.md`](docs/TESTING.md) is the manual procedure behind it, for when
you need a topology the script does not expose.
Adding another object store backend is [`docs/EXTENDING.md`](docs/EXTENDING.md).

### Without the dev environment

```bash
make test          # generate, fmt, vet, unit tests — hermetic, no cluster needed
make docker-build  # build the image
make deploy        # apply to the current context
```

`docker-build` needs BuildKit — `docker buildx`, which on macOS is
`brew install docker-buildx` plus a `cliPluginsExtraDirs` entry in
`~/.docker/config.json`. The Dockerfile mounts the Go module and build caches,
without which every image build recompiles cel-go, controller-runtime and the
Kubernetes libraries from source in a fresh container. That is the difference
between an 11-minute rebuild and a 30-second one.

The storage layer has integration tests that run against a real S3-compatible
server. They skip unless `CHRONICLE_S3_ENDPOINT` is set, so `go test ./...`
stays hermetic. The dev environment publishes its store on `localhost:19000`,
so with it running:

```bash
CHRONICLE_S3_ENDPOINT=http://localhost:19000 \
CHRONICLE_S3_ACCESS_KEY=chronicle CHRONICLE_S3_SECRET_KEY=chronicle123 \
  go test ./internal/store/ -run Integration -v
```

## Contributing

Build and test instructions, the hermetic-test story, and what is deliberately
out of scope are in [CONTRIBUTING.md](CONTRIBUTING.md). To report a suspected
vulnerability, see [SECURITY.md](SECURITY.md) — please use GitHub's private
reporting rather than a public issue.

## License

Apache-2.0. See [LICENSE](LICENSE).
