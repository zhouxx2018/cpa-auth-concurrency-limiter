# CPA Auth Concurrency Limiter Plugin Source

This repository is a third-party CPA plugin source for `auth-concurrency-limiter`.

## 1. Prepare the repository

Replace the placeholders in these files:

- `registry.json`
- `store-config.example.yaml`

Use your real GitHub repository URL, for example:

```json
"repository": "https://github.com/zhouxx2018/cpa-auth-concurrency-limiter"
```

The CPA plugin store URL will be:

```text
https://raw.githubusercontent.com/zhouxx2018/cpa-auth-concurrency-limiter/main/registry.json
```

## 2. Push and publish a release

```bash
git init
git add .
git commit -m "Initial auth concurrency limiter plugin source"
git branch -M main
git remote add origin https://github.com/zhouxx2018/cpa-auth-concurrency-limiter.git
git push -u origin main

git tag v0.1.0
git push origin v0.1.0
```

The GitHub Actions workflow builds and uploads:

```text
auth-concurrency-limiter_0.1.0_linux_amd64.zip
auth-concurrency-limiter_0.1.0_linux_arm64.zip
checksums.txt
```

CPA expects exactly these release asset names.

## 3. Configure CPA

In Docker, mount the plugin directory as writable because CPA downloads plugin files from the panel:

```bash
-v /opt/cpa/plugins:/CLIProxyAPI/plugins
```

Do not use `:ro` for panel-installed plugins.

Add your plugin source to `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "/CLIProxyAPI/plugins"
  store-sources:
    - "https://raw.githubusercontent.com/zhouxx2018/cpa-auth-concurrency-limiter/main/registry.json"
  configs:
    auth-concurrency-limiter:
      enabled: true
      priority: 1
      default_limit: 1
      slot_ttl: 15m
      strategy: round-robin
      read_auth_limits: true
```

Restart CPA, then open the CPA panel plugin store and install `Auth Concurrency Limiter`.

## 4. Manual release endpoint

```bash
curl -X POST "http://127.0.0.1:8317/v0/management/plugins/auth-concurrency-limiter/release" \
  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
  -H "content-type: application/json" \
  -d '{"auth_id":"AUTH_ID"}'
```

Check status:

```bash
curl -H "Authorization: Bearer <MANAGEMENT_KEY>" \
  http://127.0.0.1:8317/v0/resource/plugins/auth-concurrency-limiter/status
```
