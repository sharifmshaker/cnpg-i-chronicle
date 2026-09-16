# Upstream findings

Behaviour in CloudNativePG and CNPG-I that shaped this plugin's design, found
while building it and verified against source rather than inferred from
symptoms. Each entry says what we do about it and how to tell when it changes.

Versions checked: CloudNativePG **1.30.0** and `main`; CNPG-I **v0.6.0**;
plugin-barman-cloud **v0.14.0**.

None of these are filed.

---

## 1. `Operator.MutateCluster` is never invoked

**Where** CloudNativePG. A client method exists
(`internal/cnpi/plugin/client/cluster.go`), but **no reconciler calls it** —
`grep '\.MutateCluster('` outside the client itself returns nothing.

Upstream [#9149](https://github.com/cloudnative-pg/cloudnative-pg/issues/9149)
asked for it to be wired up and was closed `not_planned`.

**What we do** Restore-at-creation runs in our own `MutatingWebhookConfiguration`
rather than through the plugin protocol. That turned out better regardless:
admission at `CREATE` can set fields CloudNativePG makes immutable afterwards —
shrinking `storage.size`, changing `storageClass`, setting `postgresUID` — none
of which a plugin mutating an existing Cluster could do.

---

## 2. `Deregister` is never invoked, and has no client at all

**Where** CloudNativePG. CNPG-I defines the RPC
(`proto/operator.proto`: *"invoked when the plugin is removed from the cluster
definition"*), but the operator has **no implementation whatsoever** — the name
appears in the plugin client only inside `suite_test.go`.

**What we do** Nothing durable is keyed to plugin removal, so there is nothing
to clean up. This is why the capture watermark lives in the Cluster's own
`.status.pluginStatus` rather than in a per-cluster map on the `ConfigStore`: a
map there would grow for the lifetime of the store, and with no `Deregister`
the only way to bound it would be a periodic sweep against live Clusters. On
the Cluster, it goes away with the Cluster.

---

## 3. Plugin Services must live in the operator's namespace, and fail silently otherwise

**Where** CloudNativePG — `internal/controller/plugin_predicates.go`:

```go
func isPluginService(object client.Object, operatorNamespace string) bool {
    if object.GetNamespace() != operatorNamespace {
        return false
    }
    ...
```

A plugin Service anywhere else is ignored with nothing logged about it. The
symptom is a Cluster stuck in `PhaseUnknownPlugin` with no indication why.

**What we do** `kubernetes/` deploys into `cnpg-system`, and both
`docs/DEV-ENVIRONMENT.md` and `docs/TESTING.md` call this out, because the
symptom points nowhere near the cause.

---

## 4. An unreachable plugin halts reconciliation for every Cluster that lists it

**Where** CloudNativePG — a Cluster naming a plugin the operator cannot reach
enters `PhaseUnknownPlugin` and stops reconciling. This is deliberate and
defensible, not a bug, but it is the single most important consequence of
adopting any CNPG-I plugin and is easy to miss.

**What we do** Documented in the README's Limitations. It is also why the
restore webhook scopes itself with `matchConditions`: a Cluster that does not
ask for a restore never reaches our admission path, so the blast radius of the
plugin being down is bounded to clusters that actually opted in — though the
`PhaseUnknownPlugin` exposure applies to any Cluster listing the plugin,
including capture-only ones.
