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

## Install

```bash
go install git.erwanleboucher.dev/eleboucher/apoci/cmd/apoci@latest
```

Or build from source:

```bash
make build        # binary at ./bin/apoci
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

```bash
apoci serve
```

`apoci serve` looks for `apoci.yaml` in the current directory; override with `-c` or `APOCI_CONFIG`. The full set of options is documented in [`configs/apoci.example.yaml`](configs/apoci.example.yaml). Every field has an `APOCI_*` env var equivalent; env vars take precedence.

On first run, two tokens are generated under `dataDir`: `registry.token` (push) and `admin.token` (federation/admin API).

## Push

```bash
TOKEN=$(cat ~/apoci/data/registry.token)
docker login foo.com -u registry -p "$TOKEN"
docker push foo.com/myapp:v1
```

If you don't have TLS yet, either set up a TLS reverse proxy or add `"insecure-registries": ["foo.com:5000"]` to your Docker daemon config.

### Other backends

| Backend | Route | Push |
|---------|-------|------|
| OCI | `/v2/` | `docker push foo.com/myapp:v1` |
| npm | `/npm/` | `npm publish --registry https://foo.com/npm/` |
| Cargo | `/cargo/` | `cargo publish --registry apoci` |
| PyPI | `/pypi/` | `twine upload --repository apoci dist/*` |
| NuGet | `/nuget/` | `dotnet nuget push pkg.nupkg --api-key $TOKEN --source https://foo.com/nuget/v3/index.json` |
| Go modules | `/goproxy/` | `curl -X PUT -H "Authorization: Bearer $TOKEN" --data-binary @mod.zip https://foo.com/goproxy/<module>/@v/<version>.zip` |

All six use the same registry token and federate over the same channel. Per-backend overrides (disable, opt out of federation, separate token) live under `backends.*` in the config.

NuGet clients need the source registered first:

```bash
dotnet nuget add source https://foo.com/nuget/v3/index.json \
  --name apoci --username apoci --password "$TOKEN" --store-password-in-clear-text
```

### Go modules (goproxy)

The `/goproxy/` backend serves the [Go module proxy protocol](https://go.dev/ref/mod#goproxy-protocol) as both a store and a pull-through cache. Go has no native publish command, so push a [module zip](https://go.dev/ref/mod#zip-files) with an authed `PUT` (`.mod` and `.info` are derived from the zip). Set `backends.goproxy.upstreams` to pull-through-cache an upstream like `https://proxy.golang.org`.

Privately-hosted modules aren't in `sum.golang.org`, so clients must opt them out of checksum-DB verification:

```bash
export GOPROXY=https://foo.com/goproxy
export GOPRIVATE='your.private/*'   # or GOSUMDB=off to skip sum verification entirely
go get your.private/module@v1.0.0
```

```yaml
backends:
  goproxy:
    enabled: true
    federate: true
    upstreams:
      - https://proxy.golang.org
```

## Repository naming

Repos are namespaced by domain, the same idea as `@user@domain` in the fediverse. Two operators on different domains can never clash.

```
docker pull <node>/<origin-domain>/<image>:<tag>
```

If you operate `foo.com` and follow `bar.com`:

```bash
docker pull foo.com/foo.com/myapp:v1     # your image, from your node
docker pull bar.com/bar.com/myapp:v1     # bar's image, directly
docker pull foo.com/bar.com/myapp:v1     # bar's image, served by your node (federated)
```

The third case is the point: foo follows bar, so foo has bar's metadata. On pull, foo fetches the blob from bar, verifies the digest, caches it, and serves it. Next pull is local.

You don't need to type your own domain prefix on push — it's added automatically:

```bash
docker push foo.com/myapp:v1             # stored as foo.com/myapp
docker push foo.com/foreign.dev/app:v1   # rejected: foreign domain
```

Pulls require the repository to exist (created by a prior push or federation).

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

Each backend emits its own ActivityPub activities on write; peers ingest them and replicate file bytes in the background. If a blob hasn't replicated yet, the download handler 302s to a peer that has it. See [`docs/federation.md`](docs/federation.md) for the full activity contract.

### Manage follows

```bash
apoci follow list
apoci follow outgoing
apoci follow reject bar.com
apoci follow remove bar.com
apoci actor list
apoci identity show
```

These admin subcommands accept `--remote` and `--token` (or `APOCI_REMOTE_URL` / `APOCI_ADMIN_TOKEN`) to target a remote node, useful for headless or containerized instances.

### Retention

Without retention, every push of `:latest` leaves an orphan manifest behind, and peers mirror them all. Configure GC to drop old tags and reap untagged manifests:

```yaml
gc:
  interval: 6h
  blobGCGracePeriod: 1h
  untaggedManifestAge: 168h    # 7 days
  retention:
    keepLastN: 5
    maxAge: 720h               # 30 days
    pinnedGlobs: ["latest", "v*"]
    perRepo:
      - repo: "foo.com/myapp"
        keepLastN: 10
```

Pinned globs are never deleted and don't count against `keepLastN`. Resolution order: `perRepo` config → DB column overrides → global default. Tag and manifest deletes federate to peers, which free their copies on the next GC cycle. Run a GC cycle on demand with `apoci gc run`.

Tags are freely overwritable — re-pushing a tag repoints it at the new manifest.

### Per-follower filters

By default a follower mirrors every push. To restrict a follower to specific tag globs:

```bash
apoci follow filter bar.com --tag "latest,v*"
apoci follow filter bar.com --clear
```

Globs use `path.Match` syntax. Blob announces, manifest deletes, and untagged manifest pushes always pass the filter so peers don't end up with dangling references.

To silence federation for a repo across all followers, set `federation.excludedRepos` in the config (globs use `path.Match`). Matched repos skip outbound publish entirely — no activity row, no fan-out.

## Upstream proxy

apoci can act as a pull-through cache for any external OCI registry. Pull through your node using the upstream hostname as the leading path segment:

```bash
docker pull foo.com/docker.io/library/nginx:latest
docker pull foo.com/ghcr.io/user/repo:tag
```

First pull caches the manifest and blobs locally; subsequent pulls don't touch the upstream.

```yaml
upstreams:
  enabled: true
  registries:
    - name: docker.io
      endpoint: "https://registry-1.docker.io"
      auth: token
    - name: ghcr.io
      endpoint: "https://ghcr.io"
      auth: token
      username: myuser
      password: "ghp_yourtoken"
      private: true    # require registry token to pull cached images from this upstream
```

`auth` is `none`, `basic`, or `token` (Docker Bearer challenge). If an upstream goes unreachable, a circuit breaker opens and pulls return 404 immediately until the next probe succeeds.

`private: true` is enforced per upstream registry, not per image. If you proxy private packages from a host that also serves public packages (GHCR, Docker Hub), all cached images from that upstream require auth.

Drop a cached upstream repo with `apoci mirror evict <repo>` (add `--digest sha256:…` for a single manifest); the upstream is untouched.

## Web UI

Browser-based image browser at the root path:

```yaml
ui:
  enabled: true
```

Read-only listing of your images and federated images, with copy-paste pull commands. Repos marked `private: true` in the database are excluded. From the CLI, `apoci images list` prints hosted images and their sizes.

## Deploy

```bash
make docker
docker run -d -p 5000:5000 \
  --user 1000:1000 \
  -v ~/apoci/data:/apoci/storage \
  -v $(pwd)/apoci.yaml:/apoci/config/apoci.yaml:ro \
  apoci:latest
```

Compose:

```bash
docker compose up --build -d                           # SQLite
docker compose -f docker-compose.postgres.yml up -d    # Postgres
```

### Split-domain

Run on `registry.example.com` while your handle is `@registry@example.com`:

```yaml
endpoint: "https://registry.example.com"
accountDomain: "example.com"
```

Repos are namespaced under `example.com/*`. Proxy `/.well-known/webfinger` from the vanity domain to the service:

```
example.com {
    handle /.well-known/webfinger {
        reverse_proxy registry.example.com:443
    }
    respond 404
}
```

## Backup

Back up `dataDir` (default `/apoci/storage`). It contains the SQLite database (use `pg_dump` for Postgres), blob storage, the AP keypair, and both tokens. Restore by stopping the node, replacing the directory, and restarting.

Schema migrations run on startup. Peer version skew is tolerated within the same major version.

## Notifications and metrics

Send alerts to Discord, Slack, Telegram, email, etc. via [shoutrrr](https://github.com/nicholas-fedor/shoutrrr) URLs:

```yaml
notifications:
  urls:
    - "discord://token@id"
  events:
    - peer_health
    - follow_request
    - replication_failure
    - gc_error
```

Metrics: set `metrics.enabled: true` to expose Prometheus metrics at `/metrics` on the metrics port, optionally protected by a bearer `metrics.token`.

## Security

- Follow gate: only approved peers can send activities
- HTTP Signatures (RSA-SHA256), 5-minute validity window, replay cache
- Namespace enforcement: writes scoped to the node's domain, reads require the repo to exist
- Origin ownership: a followed peer can only write repos it created
- SHA-256 verified on every blob fetch
- SSRF protection: private IPs blocked after DNS resolution
- Rate limiting: mutating OCI requests (default 50 req/s per IP, burst 100) and the federation inbox (30 req/s, burst 100); both tunable under `rateLimits`

## Contributing

Bug reports and feature requests: open an issue on the repository.

```bash
make test
make lint
```

## License

MIT
