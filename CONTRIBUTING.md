# Contributing

Thanks for looking. This is a small project maintained best-effort — see
[Status](README.md#status) for what that means in practice.

## Building and testing

```sh
make test          # go test ./... — hermetic: no cluster, no network, no envtest
make vet fmt
make manifests generate   # regenerate CRDs, RBAC and deepcopy after changing types
make build
```

The unit suite talks to nothing. The S3 tests in `internal/store` are skipped
unless `CHRONICLE_S3_ENDPOINT` is set, so `go test ./...` is safe to run
anywhere:

```sh
CHRONICLE_S3_ENDPOINT=http://localhost:19000 \
CHRONICLE_S3_ACCESS_KEY=chronicle CHRONICLE_S3_SECRET_KEY=chronicle123 \
  go test ./internal/store/ -run Integration -v
```

If you change a `+kubebuilder:` marker, run `make manifests generate` and commit
the result. CI regenerates and fails on a dirty tree.

## Running it against a real cluster

```bash
make dev-up
```

One command builds a complete disposable environment: a kind cluster running
CloudNativePG from main, the barman-cloud plugin, an S3-compatible object store,
this plugin built from your working tree, and a PostgreSQL cluster wired to all
of it. It finishes by printing the commands to inspect what it wrote to the
bucket.

```bash
make dev-reload    # rebuild the plugin and restart it in place, ~30s
make dev-history   # the captured configuration history
make dev-down      # delete it
```

`hack/dev/scenarios/` holds runnable examples — restore, CEL transforms,
point-in-time alignment, and the cases that are deliberately refused. Each is a
single `kubectl apply`.

[`docs/DEV-ENVIRONMENT.md`](docs/DEV-ENVIRONMENT.md) documents every step the
script takes and the environment traps worth knowing about — chiefly the inotify
limit, which breaks a kind cluster in a way that looks like anything but its
cause.

[`docs/TESTING.md`](docs/TESTING.md) is the manual procedure the script
automates. Reach for it when you need a topology `up.sh` does not expose.

[`docs/UPSTREAM.md`](docs/UPSTREAM.md) is worth reading before you wonder why
something is built the way it is. Several design decisions here are worked
around CloudNativePG behaviour rather than chosen freely, and that file records
which, with the evidence.

## Adding an object-store backend

`docs/EXTENDING.md` describes the seam. `internal/store` is deliberately narrow:
a backend implements read, write and list over a prefix, and nothing above it
knows which cloud it is talking to.

## Pull requests

- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`, `chore:`). Release notes and version bumps are
  derived from them.
- New behaviour needs a test. The suite is fast and hermetic; there is no reason
  not to.
- Comments explain *why*, not *what*. Much of this codebase concerns CloudNativePG
  behaviour that is not obvious from its API — when you work something out the
  hard way, leave the finding behind for the next person.

## Scope

Some things are deliberately out of scope; the
[Limitations](README.md#limitations) section says which and why. If you want to
change one of those decisions, open an issue before writing code.
