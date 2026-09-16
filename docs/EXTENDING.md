# Adding an object store backend

Only S3-compatible stores are implemented. This is what it takes to add another,
and why each seam is where it is.

Every step below is **additive or relaxing**. Nothing here changes the shape of
an existing `ConfigStore`, so adding a backend cannot break a store already in
the field.

## The five seams

### 1. Credentials on the CRD — `api/v1/objectstore_config.go`

Add a sibling to `S3Credentials`:

```go
// AzureCredentials authenticates against Azure Blob Storage.
// +optional
AzureCredentials *AzureCredentials `json:"azureCredentials,omitempty"`
```

`S3Credentials` is already an optional pointer for exactly this reason, so no
existing field changes. Widen the exactly-one-of rule on
`ObjectStoreConfiguration`:

```go
// +kubebuilder:validation:XValidation:rule="[has(self.s3Credentials), has(self.azureCredentials)].filter(x, x).size() == 1",message="exactly one credential block must be set"
```

Going from "s3Credentials is required" to "exactly one of" only ever admits more
objects, so every stored store stays valid.

Keep the JSON field names identical to barman-cloud's. That is what lets a
credentials block be copied from an existing backup configuration unchanged, and
what makes step 4 a decode rather than a translation.

### 2. Destination pattern — same file

```go
// +kubebuilder:validation:Pattern=`^(s3|azure)://.+`
DestinationPath string `json:"destinationPath"`
```

Widening a pattern admits more values; it cannot invalidate a stored one.
`store.ParseDestination` already accepts `s3`, `azure` and `gs`, so it needs no
change — the schema is the only thing holding the line.

### 3. Backend dispatch — `internal/store/resolver.go`

`Resolver.Backend` switches on the parsed scheme. Add a case:

```go
case "azure":
    return r.azureBackend(ctx, namespace, resolved)
```

The existing `case "azure", "gs"` arm returns "not supported yet"; move `gs` out
of it and leave the arm in place until it is empty.

### 4. Derivation — `internal/store/barman.go`

`deriveConfiguration` currently refuses a barman `ObjectStore` that uses a
provider this plugin cannot talk to, so the `ConfigStore` reports why it is not
Ready rather than failing when the first snapshot is written. Remove the
provider from that guard; because the JSON names match, the decode into the
narrow type then picks the credentials up with no further work.

`derivedFrom` bypasses our CRD validation — it reads someone else's resource —
so this guard is the only check on that path. Keep it honest.

### 5. The backend itself — `internal/store/`

Implement five methods:

```go
type Backend interface {
    Check(ctx context.Context, prefix string) error        // ErrBucketNotFound when absent
    Get(ctx context.Context, key string) ([]byte, error)   // ErrNotFound when absent
    Put(ctx context.Context, key string, data []byte) error
    List(ctx context.Context, prefix string) ([]string, error)  // lexical order
    Delete(ctx context.Context, key string) error          // absent is not an error
}
```

The behaviours the rest of the plugin depends on:

- **`Check` makes one cheap request needing no permission capture lacks.** It
  backs the `ConfigStore` Ready condition. `s3.go` lists at most one key under
  the destination path. A missing container returns `ErrBucketNotFound`, which
  is reported as Ready, because `Put` creates it. Keep request IDs out of the
  error text: it lands in a status condition, and a message that changes on
  every call rewrites the object on every resync.

- **`Get` must map a missing object onto `ErrNotFound`.** `SnapshotStore.Latest`
  distinguishes "no snapshots yet" from "the store is broken" by that error
  alone. `s3.go` shows how much variation there is in how implementations spell
  a 404.
- **`List` must return lexical order.** Snapshot keys are timestamp-first so that
  lexical order is chronological, and point-in-time selection scans rather than
  parsing every object. `s3.go` sorts explicitly rather than trusting the
  provider.
- **`Delete` of a missing key is not an error.** Pruning runs against stores that
  may be partially cleaned.
- **`Put` should create the container if it is absent**, matching barman-cloud
  and what `s3.go` does. Neither MinIO nor RustFS creates one implicitly, and a
  first write that fails on a fresh bucket is a bad first experience. If the
  create is denied, say so and name the container.

## Testing it

Tests everywhere else use `internal/store/storetest`, an in-memory `Backend`
that follows this same contract. If a new backend needs the interface to grow,
extend that fake in the same change.

`internal/store/s3_integration_test.go` is written against the `Backend`
contract, not against S3 specifically. It is skipped unless
`CHRONICLE_S3_ENDPOINT` is set, so `go test ./...` stays hermetic. The same
shape works for a new backend: stand up the emulator, gate on its own
environment variable, assert the behaviours above plus a full
`SnapshotStore` round trip including checksum verification.

Run it against a real server before believing it:

```bash
docker run -d --name minio -p 19000:9000 \
  -e MINIO_ROOT_USER=chronicle -e MINIO_ROOT_PASSWORD=chronicle123 \
  quay.io/minio/minio:latest server /data

CHRONICLE_S3_ENDPOINT=http://localhost:19000 \
CHRONICLE_S3_ACCESS_KEY=chronicle CHRONICLE_S3_SECRET_KEY=chronicle123 \
  go test ./internal/store/ -run Integration -v
```

Then the end-to-end procedure in [`TESTING.md`](TESTING.md), which is written to
work against any store.

## What deliberately is not a seam

**The object layout.** Keys are built by `store.Layout` and are the same
everywhere: `<destinationPath>/<serverName>/<prefix>/snapshots/<time>-g<gen>-<hash>.json`
plus a `latest.json` pointer. A backend that reshaped them would break
point-in-time selection, which depends on lexical order being chronological.

**The snapshot format.** `internal/snapshot` produces a checksummed document
that is byte-identical regardless of where it is stored. A backend that
transformed content — compressing, re-encoding — would fail checksum
verification on read, and correctly so.
