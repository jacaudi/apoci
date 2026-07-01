# Upstream proxy

apoci can act as a pull-through cache for any external OCI registry. Pull through your node using
the upstream hostname as the leading path segment:

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

`auth` is `none`, `basic`, or `token` (Docker Bearer challenge). If an upstream goes unreachable, a
circuit breaker opens and pulls return 404 immediately until the next probe succeeds.

`private: true` is enforced per upstream registry, not per image. If you proxy private packages from
a host that also serves public packages (GHCR, Docker Hub), all cached images from that upstream
require auth.

Drop a cached upstream repo with `apoci mirror evict <repo>` (add `--digest sha256:…` for a single
manifest); the upstream is untouched.
