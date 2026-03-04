# Fix: Per-Client Rate Limiting via TLS Sidecar

## Why

The L4 SNI proxy strips the client IP — relay-tls-proxy only sees the SNI proxy's
address, so the Go server's `clientIP()` puts **all clients into one shared
rate-limit bucket**. One attacker (or a fast test suite) blocks everyone.

```
Client 1.2.3.4 ─┐
Client 5.6.7.8 ─┤─► SNI Proxy :443 ─► relay-tls-proxy (separate pod) ─► Go server
                 │   (L4, no HTTP)      RemoteAddr=10.42.x.x for ALL
```

## Solution: nginx TLS sidecar in the same Pod

Move the TLS-terminating nginx from a separate Deployment into the **same Pod**
as the Go server. nginx connects to Go on `127.0.0.1:8080`, so the existing
`clientIP()` trusts `X-Forwarded-For` (loopback = trusted). **No Go code change needed.**

```
Client 1.2.3.4 ─► SNI Proxy :443 ─► Pod [nginx :8443 → 127.0.0.1:8080 Go]
                   (L4 passthrough)    RemoteAddr=127.0.0.1 → XFF trusted ✓
```

## What changed

### 1. Helm chart — nginx TLS sidecar (`tlsSidecar.enabled: true`)

- New sidecar container in `deployment.yaml` (nginx:1.27-alpine on port 8443)
- New ConfigMap template `tls-proxy-configmap.yaml` with nginx config
- Service exposes both port 8080 (HTTP) and 8443 (HTTPS)
- Controlled via `values.yaml` — disabled by default, enabled in production

### 2. SNI Proxy — target changed

```
relay_backend → babycamapp-relay.babycamapp-relay.svc.cluster.local:8443
```
(was: `relay-tls-proxy.babycamapp-relay.svc.cluster.local:8443`)

Simple config restored — no PROXY protocol, no split server blocks.

### 3. Standalone relay-tls-proxy — removed

The separate Deployment + Service + ConfigMap in `user-data.yaml` is deleted.

## Status

| Change | Status |
|---|---|
| Helm chart sidecar (deployment, service, configmap, values) | done |
| Infra values.yaml (`tlsSidecar.enabled: true`) | done |
| SNI proxy target updated | done |
| Standalone relay-tls-proxy removed from user-data | done |
| Go `clientIP()` | **no change needed** — loopback already trusted |
