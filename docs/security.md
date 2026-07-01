# Security model

apoci's security posture rests on an explicit follow gate, signed federation traffic, and strict
namespace/ownership enforcement on writes.

- **Follow gate:** only approved peers can send activities.
- **HTTP Signatures** (RSA-SHA256), 5-minute validity window, replay cache.
- **Namespace enforcement:** writes scoped to the node's domain, reads require the repo to exist.
- **Origin ownership:** a followed peer can only write repos it created.
- **Blob integrity:** SHA-256 verified on every blob fetch.
- **SSRF protection:** private IPs blocked after DNS resolution.
- **Rate limiting:** mutating OCI requests (default 50 req/s per IP, burst 100) and the federation
  inbox (30 req/s, burst 100); both tunable under `rateLimits`.

See also the [federation contract](federation.md) for the operator-trust model — following a peer
grants it write authority over any unused package name on your node.
