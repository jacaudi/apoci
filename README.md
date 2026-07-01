<p align="center">
  <img src="internal/server/ui/static/apoci-lockup.svg" alt="apoci" width="320">
</p>

# apoci

> **Status: Beta.** Usable in production with care. APIs may change between minor versions.

A federated, self-hostable container and package registry. Each node is a single-user multi-format registry (OCI, npm, Cargo, PyPI, NuGet) and an ActivityPub actor (`@registry@your.domain`). Push an artifact and it federates to peers who follow you. If your node goes down, your peers still have your artifacts and you can rebootstrap from any of them.

```
  foo.com               bar.com              baz.com
  ┌────────────┐       ┌────────────┐       ┌────────────┐
  │ OCI :5000  │       │ OCI :5000  │       │ OCI :5000  │
  │ SQLite+FS  │◄─────►│ SQLite+FS  │◄─────►│ SQLite+FS  │
  │ AP actor   │       │ AP actor   │       │ AP actor   │
  └────────────┘       └────────────┘       └────────────┘
```

## Features

- **One registry, many formats.** Serve OCI containers alongside npm, Cargo, PyPI, NuGet, and Go modules — one node, one token.
- **Federated by design.** Every node is an ActivityPub actor (`@registry@your.domain`): push once and your artifacts replicate to the peers who follow you. Any peer can then serve them, and you can rebootstrap from any peer if your node goes down.
- **Pull-through caching.** Proxy and cache an upstream OCI registry or Go module proxy, so external dependencies keep resolving through an upstream outage.
- **Secure by default.** Signed federation (HTTP Signatures), an explicit follow-approval gate, namespace and origin-ownership enforcement, and SSRF protection.
- **Built to operate.** Tag retention and garbage collection, a read-only web UI, shoutrrr notifications, and Prometheus metrics.
- **Runs anywhere.** One binary, backed by SQLite or Postgres, with blobs on the filesystem or S3.

## Install

Pull the container image (published per release for `linux/amd64` and `linux/arm64`):

```bash
docker pull git.erwanleboucher.dev/eleboucher/apoci:0.0.48   # see the releases page for the latest tag
```

Prefer a binary? Install with Go, or build from source:

```bash
go install git.erwanleboucher.dev/eleboucher/apoci/cmd/apoci@latest
make build        # or build from source — binary at ./bin/apoci
```

## Configure and run

```bash
cp configs/apoci.example.yaml apoci.yaml
```

```yaml
# apoci.yaml — only endpoint is required
endpoint: "https://foo.com"
```

The domain becomes your identity (`@registry@foo.com`) and your repo namespace (`foo.com/*`).

Run it — as a container, mounting your config and a data volume:

```bash
docker run -d -p 5000:5000 \
  -v ~/apoci/data:/apoci/storage \
  -v $(pwd)/apoci.yaml:/apoci/config/apoci.yaml:ro \
  git.erwanleboucher.dev/eleboucher/apoci:0.0.48
```

…or as the binary: `apoci serve`.

`apoci` looks for `apoci.yaml` in the working directory (override with `-c` or `APOCI_CONFIG`); the container reads it from the mounted `/apoci/config/apoci.yaml`. The full option set is documented in [`configs/apoci.example.yaml`](configs/apoci.example.yaml) — every field has an `APOCI_*` env var equivalent, and env vars take precedence.

On first run, two tokens are generated under `dataDir`: `registry.token` (push) and `admin.token` (federation/admin API).

See [docs/deployment.md](docs/deployment.md) for Compose, split-domain, and production options.

### Common settings

Everything past `endpoint` is optional; SQLite + filesystem is the zero-config default. The most common knobs:

```yaml
dataDir: "~/.apoci"         # database, blobs, keypair, and tokens live here — back it up
storage:
  type: local               # local (default) or s3
database:
  driver: sqlite            # sqlite (default) or postgres
  dsn: ""                   # required for postgres
tls:
  cert: /etc/apoci/tls.crt  # or terminate TLS at a reverse proxy instead
  key: /etc/apoci/tls.key
```

## How it works

Each apoci node is one process — a multi-format registry and an ActivityPub actor for a single operator domain. There is no central hub.

- **Storage** — metadata in SQLite (or Postgres); blobs on the filesystem (or S3). Back up one directory.
- **Push** — an artifact is stored under your domain namespace (`foo.com/*`) and emitted as a signed ActivityPub activity to your followers.
- **Federate** — a peer that follows you ingests the activity and replicates the file bytes in the background, so your artifacts live on their node too.
- **Pull** — served locally if present; otherwise the node 302-redirects to a peer that has the bytes, verifies the digest, caches, and serves. The next pull is local.
- **Survive** — if your node dies, followers still hold your artifacts, and you rebootstrap from any of them.

The domain is the unit of identity, namespace, and trust — see [Repository naming](#repository-naming) and [Federation](#federation) below.

## Push

```bash
TOKEN=$(cat ~/apoci/data/registry.token)
docker login foo.com -u registry -p "$TOKEN"
docker push foo.com/myapp:v1
```

If you don't have TLS yet, either set up a TLS reverse proxy or add `"insecure-registries": ["foo.com:5000"]` to your Docker daemon config.

apoci serves six artifact formats over the same token. What each supports:

| Format | Local store | Caching proxy | Federation |
|--------|:---:|:---:|:---:|
| OCI | ✓ | ✓ | ✓ |
| npm | ✓ | – | ✓ |
| Cargo | ✓ | – | ✓ |
| PyPI | ✓ | – | ✓ |
| NuGet | ✓ | – | ✓ |
| Go modules | ✓ | ✓ | ✓ |

- **Local store** — push and self-host your own artifacts of this format (a self-hosted registry you own).
- **Caching proxy** — *also* pull-through-cache an upstream registry (OCI and Go modules only).
- **Federation** — writes replicate to peers that follow you.

"Local store" is about self-hosting, not access control — artifacts you publish are pull-public (the only private-read gate is on cached OCI upstream mirrors, [docs/upstream-proxy.md](docs/upstream-proxy.md)). Push commands and per-backend configuration: [docs/backends.md](docs/backends.md).

## Repository naming

Repos are namespaced by domain, the same idea as `@user@domain` in the fediverse. Two operators on different domains can never clash. You don't type your own domain prefix — it's added automatically on both push and pull:

```bash
docker push foo.com/myapp:v1             # stored as foo.com/myapp
docker pull foo.com/myapp:v1             # your image, from your node
docker push foo.com/foreign.dev/app:v1   # rejected: foreign domain
```

To pull a **federated** image (from a peer you follow), name the origin domain explicitly:

```bash
docker pull bar.com/bar.com/myapp:v1     # bar's image, from bar directly
docker pull foo.com/bar.com/myapp:v1     # bar's image, served by your node (federated)
```

That last case is the point: foo follows bar, so foo has bar's metadata. On pull, foo fetches the blob from bar, verifies the digest, caches it, and serves it — the next pull is local. Pulls require the repository to exist (created by a prior push or federation).

<details>
<summary>Fully-qualified form</summary>

Every repo also answers to its explicit `<node>/<origin-domain>/<image>` address, so your own image is equally reachable as `foo.com/foo.com/myapp:v1`. The bare form above is a convenience — apoci re-adds your own domain automatically on pull. The fully-qualified form is what federated pulls use, and it's the one to reach for in scripts or when a repo name's first path segment contains a dot (where the bare form can't be re-expanded). The web UI shows the bare command by default with the fully-qualified form one expand away.

</details>

## Federation

Follow a peer:

```bash
apoci follow add bar.com
```

The peer must accept:

```bash
# on bar
apoci follow pending
apoci follow accept foo.com
```

By default, every follow needs operator approval. Set `federation.autoAccept: mutual` to auto-accept peers you already follow, `all` for a public profile, or list specific domains in `federation.allowedDomains`.

Each backend emits its own ActivityPub activities on write; peers ingest them and replicate file bytes in the background. If a blob hasn't replicated yet, the download handler 302s to a peer that has it.

See [docs/federation.md](docs/federation.md) for follow management, per-follower filters, and the full activity contract.

## Security

- Follow gate: only approved peers can send activities
- HTTP Signatures (RSA-SHA256), 5-minute validity window, replay cache
- Namespace enforcement (writes scoped to the node's domain) and origin ownership (a peer can only write repos it created)
- SHA-256 verified on every blob fetch; SSRF protection blocks private IPs after DNS resolution
- Rate limiting on mutating OCI requests and the federation inbox

See [docs/security.md](docs/security.md) for the full model.

## Documentation

In-depth guides live in [`docs/`](docs/):

- [Package backends](docs/backends.md) — npm, Cargo, PyPI, NuGet, and Go modules (goproxy)
- [Federation](docs/federation.md) — follow management, per-follower filters, and the activity contract
- [Retention and garbage collection](docs/retention.md) — GC, tag retention, pinned globs
- [Upstream proxy](docs/upstream-proxy.md) — pull-through cache for external registries
- [Web UI](docs/web-ui.md) — the browser image browser
- [Deployment and operations](docs/deployment.md) — Docker, Compose, split-domain, backup
- [Notifications and metrics](docs/observability.md) — alerts and Prometheus
- [Security model](docs/security.md) — the full security posture

## Contributing

Bug reports and feature requests: open an issue on the repository.

```bash
make test
make lint
```

## License

MIT
