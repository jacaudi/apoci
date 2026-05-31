# apoci Helm chart

Deploys [apoci](https://git.erwanleboucher.dev/eleboucher/apoci) — a federated,
self-hostable multi-format (OCI, npm, Cargo, PyPI) registry that publishes
artifacts as an ActivityPub actor.

## Install

```sh
helm install apoci ./charts/apoci \
  --set config.endpoint=https://registry.example.com \
  --set ingress.enabled=true \
  --set ingress.hosts[0].host=registry.example.com
```

## Single-node by design

apoci uses SQLite, in-process queues, and a local blob store, all of which
assume a single writer. **Keep `replicaCount` at 1.** The Deployment uses the
`Recreate` strategy so a rollout never runs two pods against one volume.
Multi-replica HA is not yet supported even with Postgres + S3 backends.

## Configuration

`config` is rendered verbatim into `/apoci/config/apoci.yaml`. Put the full
apoci configuration there. See `configs/apoci.example.yaml` in the repo for all
options.

Tokens are best set via `auth` rather than in `config`, so they land in a
Secret and are injected as env vars (which override the config file):

```yaml
auth:
  registryToken: "..."   # or set auth.existingSecret with keys registryToken/adminToken
  adminToken: "..."
```

When no tokens are provided, apoci generates them on first start and persists
them in the data volume (read them from `/apoci/storage/{registry,admin}.token`).

## Key values

| Key | Default | Description |
| --- | --- | --- |
| `image.repository` | `git.erwanleboucher.dev/eleboucher/apoci` | Image repo |
| `image.tag` | `""` (chart appVersion) | Image tag |
| `config` | see `values.yaml` | Rendered into `apoci.yaml` |
| `auth.registryToken` / `auth.adminToken` | `""` | Tokens; empty = auto-generate |
| `auth.existingSecret` | `""` | Secret with `registryToken`/`adminToken` keys |
| `persistence.enabled` | `true` | Create a PVC for `/apoci/storage` |
| `persistence.size` | `50Gi` | PVC size |
| `persistence.existingClaim` | `""` | Use an existing PVC instead |
| `ingress.enabled` | `false` | Create an Ingress for the registry port |
| `serviceMonitor.enabled` | `false` | Prometheus Operator ServiceMonitor (requires `config.metrics.enabled`) |
