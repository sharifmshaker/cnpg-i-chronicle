# Examples

| File | What it shows |
|---|---|
| `configstore-derived.yaml` | A store that reuses a barman-cloud `ObjectStore` |
| `configstore-standalone.yaml` | A store with no dependency on the barman-cloud plugin |
| `cluster-save.yaml` | A cluster capturing its configuration on every generation change |
| `cluster-restore.yaml` | A cluster inheriting configuration from another cluster's history |
| `cluster-restore-at-time.yaml` | Restoring the configuration in force at a given moment |
| `cluster-restore-pitr.yaml` | Deriving that moment from the cluster's own recovery target |
| `cluster-restore-transform.yaml` | Rescaling captured values with CEL on the way in |

`select.mode` offers `Latest`, `Generation`, `Timestamp` and
`AlignWithRecoveryTarget`.
