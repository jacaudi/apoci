# Package backends

apoci is a single-user, multi-format registry. Alongside OCI containers (`/v2/`), it serves five
package backends — npm, Cargo, PyPI, NuGet, and Go modules. All six use the same registry token
and federate over the same channel. Per-backend overrides (disable, opt out of federation, separate
token) live under `backends.*` in the config (see [Per-backend configuration](federation.md#per-backend-configuration)).

## Supported formats

| Format | Route | Local store | Caching proxy | Federation | Push |
|--------|-------|:---:|:---:|:---:|------|
| OCI | `/v2/` | ✓ | ✓ | ✓ | `docker push foo.com/myapp:v1` |
| npm | `/npm/` | ✓ | – | ✓ | `npm publish --registry https://foo.com/npm/` |
| Cargo | `/cargo/` | ✓ | – | ✓ | `cargo publish --registry apoci` |
| PyPI | `/pypi/` | ✓ | – | ✓ | `twine upload --repository apoci dist/*` |
| NuGet | `/nuget/` | ✓ | – | ✓ | `dotnet nuget push pkg.nupkg --source https://foo.com/nuget/v3/index.json` |
| Go modules | `/goproxy/` | ✓ | ✓ | ✓ | authed `PUT` (see [below](#go-modules-goproxy)) |

- **Local store** — push and self-host your own artifacts of this format. Every format apoci serves is a self-hosted registry you own; the OCI and goproxy backends *additionally* proxy an upstream.
- **Caching proxy** — pull-through cache of an upstream registry. OCI proxies external OCI registries ([upstream-proxy.md](upstream-proxy.md)); goproxy proxies an upstream Go module proxy. The other four are store-only.
- **Federation** — writes replicate to peers that follow you (togglable per backend via `backends.<name>.federate`).

> **"Local store" is about self-hosting, not access control.** Artifacts you publish are pull-public — apoci has no per-package read-auth for your own artifacts. The only private-read gate is on *cached OCI upstream mirrors* (`private: true` per upstream — see [upstream-proxy.md](upstream-proxy.md)), which require the registry token to pull the cached copy.

`$TOKEN` is the registry token (`{dataDir}/registry.token`).

## NuGet

NuGet clients need the source registered first:

```bash
dotnet nuget add source https://foo.com/nuget/v3/index.json \
  --name apoci --username apoci --password "$TOKEN" --store-password-in-clear-text
```

## Go modules (goproxy)

The `/goproxy/` backend serves the [Go module proxy protocol](https://go.dev/ref/mod#goproxy-protocol) as both a store and a pull-through cache. Go has no native publish command, so push a [module zip](https://go.dev/ref/mod#zip-files) with an authed `PUT` (`.mod` and `.info` are derived from the zip). Set `backends.goproxy.upstreams` to pull-through-cache an upstream like `https://proxy.golang.org`.

Public modules pulled through the cache are still verified by your client's `GOSUMDB` (apoci does not verify upstream content itself), so leave checksum-DB verification on. Privately-hosted modules aren't in `sum.golang.org`, so opt only those out:

```bash
export GOPROXY=https://foo.com/goproxy
export GOPRIVATE='your.private/*'   # opts private modules out of GOSUMDB; keep it enabled for public ones
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
