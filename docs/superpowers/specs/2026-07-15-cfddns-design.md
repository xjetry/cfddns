# cfddns — Cloudflare DDNS (Go)

**Date:** 2026-07-15  
**Status:** Approved for implementation planning

## Goal

A single Go binary that keeps Cloudflare DNS A/AAAA records in sync with the host’s public IPv4/IPv6 address. Supports periodic updates, optional HTTP manual trigger, CLI one-shot update, and systemd install/uninstall for one-liner deploy via `wget` + `-install`.

## Non-goals

- Multi-provider DNS (Cloudflare only)
- Global API Key + email auth
- Orange-cloud (proxied) records
- Web UI / complex config file formats beyond install-time env

## CLI

Binary name: `cfddns`

| Flag | Env (install) | Required | Default | Description |
|------|---------------|----------|---------|-------------|
| `-4` | `CFDDNS_V4` | one of `-4`/`-6` | — | FQDN for A record |
| `-6` | `CFDDNS_V6` | one of `-4`/`-6` | — | FQDN for AAAA record |
| `-token` | `CFDDNS_TOKEN` | yes (run/once) | — | Cloudflare API Token |
| `-period` | `CFDDNS_PERIOD` | no | `5m` | Update interval (`time.ParseDuration`) |
| `-port` | `CFDDNS_PORT` | no | unset | If set, start HTTP server on this port |
| `-passwd` | `CFDDNS_PASSWD` | with `-port` | — | Bearer secret for HTTP `/update` |
| `-once` | — | no | false | Run one update cycle and exit |
| `-install` | — | no | false | Install binary + env + systemd unit |
| `-uninstall` | — | no | false | Stop service, remove unit/env/binary |

### Modes

1. **Daemon (default):** validate → update once → ticker loop; optional HTTP if `-port` set.
2. **`-once`:** validate → update once → exit 0 if every configured family succeeds (including “unchanged”); exit non-zero if any configured family fails to detect IP or update DNS.
3. **`-install`:** requires root; copies self to `/usr/local/bin/cfddns`, writes `/etc/cfddns.env` (mode `0600`) from flags, writes systemd unit, `daemon-reload`, `enable --now`.
4. **`-uninstall`:** requires root; `systemctl stop/disable`, remove unit + env + `/usr/local/bin/cfddns`, `daemon-reload`.

`-install` / `-uninstall` are mutually exclusive with each other and with daemon/`-once` runtime (install writes config; does not stay running as the long-lived process beyond starting the unit).

### Validation

- At least one of v4 domain or v6 domain must be set.
- Token required for daemon and `-once`.
- `period` must parse as duration and be `> 0`.
- If `-port` is set, `-passwd` is required (non-empty).
- If `-passwd` is set without `-port`, error (misconfiguration).
- Domain values must be non-empty FQDN-like strings (basic check: contains `.`, no spaces).
- `-install` must receive the same domain/token/period/optional port/passwd that should be persisted.

### Config resolution

Merge order (higher wins):

1. Defaults (`period=5m`)
2. If `/etc/cfddns.env` exists and is readable, load into process env (for `sudo cfddns -once` after install; under systemd the unit already injects the same file)
3. Process environment (`CFDDNS_*`)
4. CLI flags

Only keys present in the env file / flags override lower layers.

## Public IP detection

Use Cloudflare trace with forced address family:

| Family | URL |
|--------|-----|
| IPv4 | `https://1.1.1.1/cdn-cgi/trace` with TCP dial restricted to IPv4 |
| IPv6 | `https://[2606:4700:4700::1111]/cdn-cgi/trace` with TCP dial restricted to IPv6 |

Parse the `ip=` line from the response body.

Only probe families that have a corresponding domain configured. Timeouts: ~10s per request. Errors are logged; the process does not exit in daemon mode.

## Cloudflare DNS update

Auth: **API Token only** (`Authorization: Bearer <token>`).

API base: `https://api.cloudflare.com/client/v4`

Flow per configured family:

1. Resolve zone ID by listing zones or progressive suffix match on the FQDN (longest matching zone name wins).
2. Find DNS record by zone + type (`A` / `AAAA`) + name (FQDN).
3. If content equals current public IP → skip update.
4. If missing → `POST` create with `proxied=false`, `ttl=1` (automatic).
5. If present and different → `PUT` update content; force `proxied=false`.

Token needs: **Zone → DNS → Edit** (and Zone → Zone → Read for zone lookup).

Per-family failures do not abort the other family. Daemon continues on next period.

## Concurrency

A single mutex serializes update runs so ticker and HTTP `/update` never race on the same records.

## HTTP server (optional)

- Listen: `:<port>` (all interfaces; operator can firewall).
- Auth: `Authorization: Bearer <passwd>` (constant-time compare).
- Endpoints:
  - `POST /update` and `GET /update` — run one update cycle.
- Success JSON example:

```json
{
  "ok": true,
  "results": [
    {"family": "ipv4", "domain": "home.example.com", "ip": "1.2.3.4", "action": "updated"},
    {"family": "ipv6", "domain": "home.example.com", "ip": "2001:db8::1", "action": "unchanged"}
  ]
}
```

- Unauthorized → 401; update soft failures reflected in `ok` / per-result errors with HTTP 200 or 502 as appropriate (prefer 200 with `ok:false` for partial IP/API errors so clients can parse body).

No HTTP server when `-port` is unset.

## systemd install layout

**Binary:** `/usr/local/bin/cfddns`

**Env file:** `/etc/cfddns.env` (mode `0600`, owner root)

```
CFDDNS_V4=...
CFDDNS_V6=...
CFDDNS_TOKEN=...
CFDDNS_PERIOD=5m
CFDDNS_PORT=...
CFDDNS_PASSWD=...
```

Empty optional keys may be omitted.

**Unit:** `/etc/systemd/system/cfddns.service`

```
[Unit]
Description=Cloudflare DDNS (cfddns)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/cfddns.env
ExecStart=/usr/local/bin/cfddns
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

**Install sequence:** copy binary → write env → write unit → `systemctl daemon-reload` → `systemctl enable --now cfddns`

**Uninstall sequence:** `systemctl stop/disable cfddns` → remove unit, env, binary → `daemon-reload`

## Deploy UX

```bash
wget -O cfddns https://example.com/releases/cfddns && chmod +x cfddns
sudo ./cfddns -install -4 home.example.com -token "$CF_TOKEN" -period 5m
# optional HTTP:
# sudo ./cfddns -install -4 home.example.com -token "$CF_TOKEN" -port 8080 -passwd secret

# manual CLI trigger after install:
sudo cfddns -once

# manual HTTP trigger:
curl -sS -H "Authorization: Bearer secret" -X POST http://127.0.0.1:8080/update
```

Release builds: static Linux amd64/arm64 binaries suitable for `wget` (document `CGO_ENABLED=0`).

## Package layout

```
cmd/cfddns/main.go
internal/config/     # flags, env, validation, modes
internal/ip/         # dual-stack Cloudflare trace client
internal/cloudflare/ # zone + DNS record client
internal/updater/    # orchestrate detect + update + mutex
internal/httpserver/ # Bearer auth + /update
internal/install/    # systemd install/uninstall
```

## Error handling & logging

- Log to stdout/stderr (journald captures under systemd).
- Include family, domain, action (created/updated/unchanged/error), and IP when relevant.
- Never log full token or passwd.

## Testing

- Unit tests: config validation; trace body parser; zone suffix matching; “skip when IP unchanged” logic with fake CF client.
- Optional integration tests behind build tag / env with real token (not required for default CI).

## Success criteria

1. With only `-4` + `-token`, A record tracks public IPv4 on the period.
2. With only `-6` + `-token`, AAAA record tracks public IPv6.
3. Both can run together with independent domains.
4. Unchanged IP does not call update API unnecessarily.
5. `-port` + `-passwd` exposes authenticated manual update.
6. `-once` performs one cycle (works with `/etc/cfddns.env` after install).
7. `-install` / `-uninstall` manage systemd lifecycle on Linux.
8. Invalid flag combinations fail fast with clear errors.
