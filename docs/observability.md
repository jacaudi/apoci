# Notifications and metrics

## Notifications

Send alerts to Discord, Slack, Telegram, email, etc. via
[shoutrrr](https://github.com/nicholas-fedor/shoutrrr) URLs:

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

## Metrics

Set `metrics.enabled: true` to expose Prometheus metrics at `/metrics` on the metrics port,
optionally protected by a bearer `metrics.token`.
