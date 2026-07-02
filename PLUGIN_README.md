# Auth Concurrency Limiter Plugin

This Go plugin limits concurrent requests per auth file in pure plugin mode.

It implements:

- `scheduler.pick`: selects only candidates whose auth-file bucket still has capacity, and acquires one in-memory slot before returning the auth ID.
- `usage.handle`: releases one slot for the completed `AuthID`.
- `management.register` / `management.handle`: exposes `/v0/resource/plugins/auth-concurrency-limiter/status` for diagnostics and `/v0/management/plugins/auth-concurrency-limiter/release` for manual slot release.

The implementation is intentionally process-local. It does not require Redis or changes to CPA core.

## Configuration

Add the plugin under `plugins.configs`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    auth-concurrency-limiter:
      enabled: true
      priority: 1
      default_limit: 0
      slot_ttl: 15m
      strategy: round-robin
      auth_refresh_interval: 30s
      read_auth_limits: true
      deny_status: 429
      limits:
        account-a.json: 2
        "C:/cliproxy/auth/account-b.json": 1
```

Fields:

- `default_limit`: max concurrency for auths without a matching configured or auth-JSON limit. `0` means unlimited.
- `limits`: per-auth limits keyed by file name, path, auth ID, or auth index. These override auth JSON fields.
- `slot_ttl`: stale-slot fallback. Defaults to `15m`.
- `strategy`: `round-robin` or `fill-first`.
- `auth_refresh_interval`: how often to refresh host auth metadata and auth JSON limits. `0` disables refresh.
- `read_auth_limits`: when enabled, reads `cpa_max_concurrency` or `max_concurrency` from auth JSON files via host callbacks.
- `deny_status`: HTTP status returned when all candidates are full. Defaults to `429`.

You can also set a limit in an auth JSON file:

```json
{
  "type": "gemini",
  "cpa_max_concurrency": 1
}
```

## Build

From `examples/plugin/auth-concurrency-limiter/go`:

```bash
go test .
go build -buildmode=c-shared -o auth-concurrency-limiter.dll .
```

Use `.so` on Linux and `.dylib` on macOS.

Copy the dynamic library to CPA's plugin directory, for example:

```text
plugins/windows/amd64/auth-concurrency-limiter.dll
plugins/linux/amd64/auth-concurrency-limiter.so
plugins/darwin/arm64/auth-concurrency-limiter.dylib
```

Restart CPA and check:

```text
GET /v0/management/plugins
GET /v0/resource/plugins/auth-concurrency-limiter/status
```

## Manual Release

Use the status resource to inspect active `slot_id`, `auth_id`, `auth_index`, and `file_key` values:

```text
GET /v0/resource/plugins/auth-concurrency-limiter/status
```

Release by auth ID:

```bash
curl -X POST "http://127.0.0.1:8317/v0/management/plugins/auth-concurrency-limiter/release" \
  -H "content-type: application/json" \
  -d '{"auth_id":"AUTH_ID"}'
```

Release one exact slot:

```bash
curl -X POST "http://127.0.0.1:8317/v0/management/plugins/auth-concurrency-limiter/release" \
  -H "content-type: application/json" \
  -d '{"slot_id":"SLOT_ID"}'
```

Release a whole auth-file bucket. `file_key` accepts the normalized key from the status resource or the auth file name:

```bash
curl -X POST "http://127.0.0.1:8317/v0/management/plugins/auth-concurrency-limiter/release" \
  -H "content-type: application/json" \
  -d '{"file_key":"account-a.json"}'
```

Clear every active slot:

```bash
curl -X POST "http://127.0.0.1:8317/v0/management/plugins/auth-concurrency-limiter/release" \
  -H "content-type: application/json" \
  -d '{"all":true}'
```

The response is JSON:

```json
{"released":1,"reason":"auth"}
```

## Behavior Notes

This is best-effort lifecycle limiting because CPA's current plugin API has no request-finished scheduler hook. Slots are acquired in `scheduler.pick` and released from `usage.handle`.

If CPA does not emit usage for a selected auth, the slot remains until `slot_ttl`. If a request runs longer than `slot_ttl`, the slot can expire before the real request completes.

CPA currently uses the first active scheduler plugin. If another scheduler plugin is enabled with higher priority, this plugin may not receive `scheduler.pick`.
