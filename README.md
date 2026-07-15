# cfddns

Single-binary Cloudflare DDNS for Linux. Keeps A/AAAA records in sync with the host’s public IPv4/IPv6 address via Cloudflare’s trace endpoint and DNS API.

Supports:

- Periodic daemon updates (default)
- One-shot CLI update (`-once`)
- Optional HTTP manual trigger (`GET`/`POST /update`)
- systemd install / uninstall (`-install` / `-uninstall`)

## Build

```bash
CGO_ENABLED=0 go build -o cfddns ./cmd/cfddns
```

## Cloudflare API token

Create a token with at least:

| Permission | Access |
|------------|--------|
| Zone → DNS | Edit |
| Zone → Zone | Read |

Scope the token to the zone(s) that own the FQDNs you will update.

## Flags

| Flag | Env (install / `/etc/cfddns.env`) | Default | Description |
|------|-----------------------------------|---------|-------------|
| `-4` | `CFDDNS_V4` | — | FQDN for A record (IPv4) |
| `-6` | `CFDDNS_V6` | — | FQDN for AAAA record (IPv6) |
| `-token` | `CFDDNS_TOKEN` | — | Cloudflare API token (required for run/once) |
| `-period` | `CFDDNS_PERIOD` | `5m` | Update interval (`time.ParseDuration`) |
| `-port` | `CFDDNS_PORT` | `0` (disabled) | HTTP listen port for `/update` |
| `-passwd` | `CFDDNS_PASSWD` | — | Bearer secret (required if `-port` is set) |
| `-once` | — | false | Run one update cycle and exit |
| `-install` | — | false | Install binary + env + systemd unit (root) |
| `-uninstall` | — | false | Stop service and remove unit/env/binary (root) |

At least one of `-4` or `-6` is required for run, once, and install. Config merge order: defaults → `/etc/cfddns.env` → process env → CLI flags.

## Install (wget + systemd)

```bash
# Replace the URL with your release binary location.
wget -O /tmp/cfddns https://example.com/releases/cfddns
chmod +x /tmp/cfddns
sudo /tmp/cfddns -install \
  -4 home.example.com \
  -6 home.example.com \
  -token "$CF_TOKEN" \
  -period 5m \
  -port 8080 \
  -passwd "$HTTP_SECRET"
```

This copies the binary to `/usr/local/bin/cfddns`, writes `/etc/cfddns.env` (mode `0600`), installs a systemd unit, and `enable --now` the service.

Uninstall:

```bash
sudo cfddns -uninstall
```

### Logs (journalctl)

```bash
journalctl -u cfddns -f
journalctl -u cfddns -n 100 --no-pager
journalctl -u cfddns --since "1 hour ago"
```

CLI help (same flags + log commands):

```bash
cfddns -h
```

## One-shot update

Useful after install (reads `/etc/cfddns.env`) or for ad-hoc runs:

```bash
cfddns -once -4 home.example.com -token "$CF_TOKEN"
# or, with env file already present:
sudo cfddns -once
```

Exit code is non-zero if any configured family fails (detect or DNS). Exit 0 only when every configured family is `created`, `updated`, or `unchanged`.

## HTTP `/update`

When `-port` and `-passwd` are set:

```bash
curl -sS -H "Authorization: Bearer $HTTP_SECRET" \
  http://127.0.0.1:8080/update
```

Response is always HTTP 200 on auth success; body includes `"ok": true|false` and per-family results. Partial failures (e.g. one family missing) set `"ok": false` without stopping the daemon.

## Missing IPv4 / IPv6 stack

If a family has no usable stack (no route, dial timeout, etc.), that family is a **soft failure**:

- Logged with family, domain, and error
- That family’s DNS record is left unchanged
- The other family (if configured) still updates
- **Daemon mode never exits** solely because IP detection fails for one or both families; the ticker and optional HTTP server keep running and retry next period
- **`-once`** exits non-zero when any configured family fails

## Modes

1. **Daemon (default):** validate → update once → optional HTTP → period ticker until SIGINT/SIGTERM
2. **`-once`:** validate → one update → exit status reflects success
3. **`-install`:** root; install binary, env, unit; start service
4. **`-uninstall`:** root; stop service; remove unit, env, binary

`-install` and `-uninstall` are mutually exclusive with each other and with `-once`.
