# Scenarios

Each file is a self-contained `kubectl apply` against a running dev
environment (`./hack/dev/up.sh`). They assume the defaults that script
creates: a store `config-store`, a bucket `chronicle`, and a source cluster
`pg-source` with a captured history.

| File | What it demonstrates | Extra steps |
|---|---|---|
| `01-restore-basic.yaml` | Restoring one cluster's configuration into another, with skip rules. Note the policy has no source — the Cluster names it | none |
| `02-restore-transform.yaml` | Rescaling captured values with CEL, and the path checks that catch typos before any cluster exists | none |
| `03-restore-at-time.yaml` | Selecting the configuration in force at a given moment | edit a timestamp |
| `04-restore-pitr.yaml` | Deriving that moment from the cluster's own recovery target — the feature the project exists for | take a backup first |
| `05-standalone-store.yaml` | A store configured directly rather than derived from a barman `ObjectStore` | none |
| `06-guardrails.yaml` | Five things that are refused — two policies by the controller, three clusters at admission | errors expected |

Read the comment block at the top of each file — it says what to run
afterwards and what to expect.

## Suggested order

```bash
# 1. See a plain restore, and the record it leaves behind.
kubectl apply -f hack/dev/scenarios/01-restore-basic.yaml
kubectl get cluster pg-restored -o jsonpath='{.spec.postgresql.parameters}' | jq

# 2. See values rescaled on the way in, with the rules checked before any cluster exists.
kubectl apply -f hack/dev/scenarios/02-restore-transform.yaml
kubectl get restorepolicy dev-sized -o jsonpath='{.status.conditions}' | jq

# 3. Build a history, then restore a point in it.
kubectl patch cluster pg-source --type merge \
  -p '{"spec":{"postgresql":{"parameters":{"shared_buffers":"256MB"}}}}'
./hack/dev/history.sh pg-source

# 4. Confirm the guardrails actually hold. Errors are the point.
kubectl apply -f hack/dev/scenarios/06-guardrails.yaml
#    Three clusters are refused at admission; two policies are created but
#    report themselves unusable:
kubectl get restorepolicy \
  -o 'custom-columns=NAME:.metadata.name,READY:.status.conditions[0].status,WHY:.status.conditions[0].message'
```

## Cleaning up between scenarios

```bash
./hack/dev/down.sh --keep    # drop the scenario clusters and policies
```

This deliberately leaves `pg-source`, the stores and the bucket alone, so the
captured history keeps growing across scenarios.

If you *do* delete `pg-source`, do not simply recreate it: its WAL archive survives
in the bucket, CloudNativePG sees a non-empty archive for that server name,
concludes the cluster is already bootstrapped, and strands it as
`unrecoverable` with no instance. Clear the archive first:

```bash
kubectl delete cluster pg-source
eval $(./hack/dev/env.sh) && rclone purge dev:chronicle/pg-source
./hack/dev/up.sh
```

Simpler, and what to reach for unless you have a reason not to:

```bash
make dev-down && make dev-up
```
