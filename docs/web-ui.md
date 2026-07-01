# Web UI

Browser-based image browser at the root path:

```yaml
ui:
  enabled: true
```

Read-only listing of your images and federated images, with copy-paste pull commands. Repos marked
`private: true` in the database are excluded. From the CLI, `apoci images list` prints hosted images
and their sizes.
