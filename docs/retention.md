# Retention and garbage collection

Without retention, every push of `:latest` leaves an orphan manifest behind, and peers mirror them
all. Configure GC to drop old tags and reap untagged manifests:

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

Pinned globs are never deleted and don't count against `keepLastN`. Resolution order: `perRepo`
config → global default. Tag and manifest deletes federate to peers, which free their copies on
the next GC cycle. Run a GC cycle on demand with `apoci gc run`.

Tags are freely overwritable — re-pushing a tag repoints it at the new manifest.
