# Deployment and operations

## Docker

Use the published image (built per release for `linux/amd64` and `linux/arm64`):

```bash
docker run -d -p 5000:5000 \
  --user 1000:1000 \
  -v ~/apoci/data:/apoci/storage \
  -v $(pwd)/apoci.yaml:/apoci/config/apoci.yaml:ro \
  git.erwanleboucher.dev/eleboucher/apoci:0.0.48
```

Or build the image from source with `make docker` (tags `apoci:<version>`).

Compose:

```bash
docker compose up --build -d                           # SQLite
docker compose -f docker-compose.postgres.yml up -d    # Postgres
```

## Split-domain

Run on `registry.example.com` while your handle is `@registry@example.com`:

```yaml
endpoint: "https://registry.example.com"
accountDomain: "example.com"
```

Repos are namespaced under `example.com/*`. Proxy `/.well-known/webfinger` from the vanity domain to
the service:

```
example.com {
    handle /.well-known/webfinger {
        reverse_proxy registry.example.com:443
    }
    respond 404
}
```

## Backup

Back up `dataDir` (default `/apoci/storage`). It contains the SQLite database (use `pg_dump` for
Postgres), blob storage, the AP keypair, and both tokens. Restore by stopping the node, replacing
the directory, and restarting.

Schema migrations run on startup. Peer version skew is tolerated within the same major version.
