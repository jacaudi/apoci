# Federation adapters

apoci federates package events over ActivityPub. Each backend (OCI, npm, Cargo, PyPI) owns its own AP object types and ports its publish/update/delete events through a `FederationAdapter` registered in the inbox.

## Wire model

The activity envelope is generic AS2:

```json
{
  "@context": "https://www.w3.org/ns/activitystreams",
  "id": "https://foo.com/ap/activities/<uuid>",
  "type": "Create" | "Update" | "Announce" | "Delete",
  "actor": "https://foo.com/ap/actor",
  "to": ["https://www.w3.org/ns/activitystreams#Public"],
  "cc": ["https://foo.com/ap/followers"],
  "object": { ... }
}
```

The `object` payload is backend-specific. Each backend defines its types in `internal/registry/<backend>/federation.go`.

## Adapter contract

```go
type FederationAdapter interface {
    PackageType() string  // "oci" | "npm" | "cargo" | "pypi"
    APTypes() []string    // AP object type names this adapter owns, e.g. ["NpmVersion", "NpmTag"]
    Ingest(ctx context.Context, activityType, apType string, obj map[string]any, actorURL string) error
}
```

The inbox dispatch picks the adapter by AP object type. `activityType` is the AS2 activity (`Create` / `Update` / `Delete` / `Announce`); `apType` is the object type name (e.g. `"CargoYank"`); `obj` is the unmarshaled object payload; `actorURL` is the verified sender's actor URL.

## Outbound

Each backend's `Backend` accepts an `activitypub.PackagePublisher` in its `Config.Publisher`. After a successful local write (publish, tag change, yank, etc.), the handler calls `b.publisher.Publish(ctx, activityType, object)`. The publisher signs, persists, and queues delivery to followers.

Federation must never block or fail a successful local write — emit warnings to the logger and move on.

## Sender authority

A peer can only act on packages it owns. Adapters call `GetOrCreatePackage(ctx, type, name, senderActorURL)` for create paths — that single transactional call atomically creates a package owned by the sender or returns `database.ErrPackageOwnerMismatch` when an existing package belongs to someone else. Update/delete paths look the package up first and reject when `OwnerID != senderActorURL` (ownership is immutable once set, so there's no TOCTOU).

The OCI flow has an extra layer: repos are namespaced by domain (`foo.com/myapp`), and the inbox enforces that the leading namespace segment matches the sender's account domain. The other backends use globally-unique package names (npm `lodash`, Cargo `serde`, PyPI `requests`); first-publish wins per name on a given peer.

## Operator-trust assumptions

Following a peer effectively grants them write authority over any unused name on this node. A peer that you've accepted can claim `lodash` in npm, `serde` in cargo, or `requests` in pypi — and once claimed, you cannot publish under that same name without unfollowing them and removing their package row. The follow-acceptance gate (`apoci follow accept`) is the only line of defense; vet peers accordingly.

OCI is unaffected: `foo.com/lodash` is scoped to whichever follower owns `foo.com`, so two peers can each have their own `lodash` repo without colliding.

## Delivery guarantees

`Publish` is best-effort. The local write commits before the activity is generated; if persisting the activity row or enqueuing follower deliveries fails, the publish is logged and dropped — peers miss that one event. There is no automatic resync. This matches the OCI behavior in `PublishManifest`/`PublishTag`/`PublishBlobRef`. Operators who need stricter durability should monitor publisher errors in logs and follow up manually.

## Out-of-order activities

Activities for a given package are not strictly ordered across peers. If `Update NpmTag`, `Update CargoYank`, or `Delete NpmVersion` arrives at a peer before the corresponding `Create` for that version, the adapter calls `lookupOwnedPackage`, sees no row, and silently drops the activity. The tag/yank/delete is permanently lost on that peer (the origin's delivery queue marks it delivered). This matches the OCI inbox behavior for tags arriving before manifests. Mitigations are out of scope for v1.

## File bytes

All four backends use the same two-tier model:

1. On ingest, the adapter records the peer in `peer_blobs` (so we know where bytes can be fetched) and asynchronously calls `BlobReplicator.ReplicateFromURL` to pull the file into the local blobstore. Replication is best-effort with a 5-minute per-blob timeout, capped concurrency, and panic recovery.
2. On a download request, the handler tries the local blobstore first. On `ErrBlobNotFound` (replication hasn't completed or failed), it 302-redirects to a peer in `peer_blobs`. If no healthy peer has it, the request 404s.

The redirect path is the safety net: even if eager replication is slow or breaks, clients still resolve to the bytes as long as one peer is up. Once replication completes, subsequent requests serve locally.

## Per-backend configuration

```yaml
backends:
  npm:
    enabled: true     # default true; false skips construction, route mount, and adapter registration
    federate: true    # default true; false omits the publisher and the inbox adapter
    token: ""         # default empty → falls back to the global RegistryToken
  cargo: { ... }
  pypi:  { ... }
```

The same fields are also accepted as env vars: `APOCI_BACKENDS_NPM_ENABLED`, `APOCI_BACKENDS_CARGO_FEDERATE`, `APOCI_BACKENDS_PYPI_TOKEN`, etc.

## Adding a new backend

1. Implement `registry.Backend` (`Type()`, `RoutePrefix()`, `Handler()`).
2. Define your AP object types in `internal/registry/<name>/federation.go`.
3. Implement `activitypub.FederationAdapter` on a struct that wraps the backend.
4. Add `Publisher activitypub.PackagePublisher` to your `Config`. Call `b.publisher.Publish` from your write handlers.
5. Wire it up in `internal/server/server.go`: instantiate the backend, register it with `pkgreg.Manager`, and register the adapter with the `*activitypub.AdapterRegistry`.
6. Add publish-emits and Ingest round-trip tests, mirroring the npm/cargo/pypi ones.

The wire format is operator-visible. Pick stable JSON-LD field names — they are part of the federation contract once peers exchange them.
