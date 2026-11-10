# HomeAutomationMonitorGW

HomeAutomationMonitorGW is a read-only Linux monitoring gateway. The single `automation-gateway` process polls configured NUT, SNMPv3, and Modbus RTU sources, retains the latest values in memory, and serves an authenticated HTTPS API. It can optionally forward cache deltas to Zabbix with `history.push`; this is built into the standard process and package, not a second application or service.

## Requirements

- Linux with systemd for packaged service operation.
- Go from `go.mod` only when building from source; prebuilt packages need no Go toolchain.
- Site-owned TLS certificate/key and bearer-token files.
- Access to the configured read-only NUT, SNMPv3, or Modbus RTU sources.
- Optional Zabbix 7.4+ API access when built-in delta forwarding is enabled.

## Install

The signed APT repository provides `automation-gateway` and the optional, independently installed `automation-gateway-led` client. Installing or upgrading a package preserves site configuration and does not enable or start a service.

```bash
sudo apt install automation-gateway
sudoedit /etc/automation-gateway/config.yaml
sudo -u automation-gateway /usr/sbin/automation-gateway \
  --config /etc/automation-gateway/config.yaml --check
sudo systemctl enable --now automation-gateway.service
```

For a release archive, verify its adjacent checksum before extraction, then run `sudo ./scripts/install.sh`.

Provision TLS keys, collector secrets, bearer tokens, and an optional Zabbix API token as protected regular files owned by root or `automation-gateway`, mode `0640` or stricter. Do not put secret values in YAML.

## Configure

Start from [`configs/config.example.yaml`](configs/config.example.yaml). It documents HTTPS, authentication, delta-journal, logging, source, and optional Zabbix settings. Unknown YAML fields are rejected.

The optional built-in Zabbix worker uses the active in-memory store directly. A minimal section is:

```yaml
zabbix:
  enabled: true
  api_url: "https://zabbix.example.invalid/api_jsonrpc.php"
  token_file: "/etc/automation-gateway/tokens/zabbix-api"
  host: "automation-gateway"
```

Defaults are a 5-second delta poll, 15-minute inventory refresh, 10-second request timeout, and cursor state at `/var/lib/automation-gateway/zabbix-cursor`. Optional `ca_file`, `batch_size`, `max_pages`, and `max_response_bytes` settings are shown in the example. When `enabled: false`, the gateway does not read a Zabbix token, open Zabbix network connections, create cursor state, or start the worker.

Import [`zabbix/template_homeauthmonitorgw.yaml`](zabbix/template_homeauthmonitorgw.yaml) manually and grant a dedicated Zabbix API token only the permissions required for `history.push` to the configured host. Package installation never creates a token, imports a template, or changes Zabbix.

## Validate and operate

Validate every candidate configuration before restart or reload:

```bash
sudo -u automation-gateway /usr/sbin/automation-gateway \
  --config /etc/automation-gateway/config.yaml --check
```

Reload creates a new cache generation, stops and waits for the old collectors and optional Zabbix worker, then starts the validated replacement generation. The worker bootstraps the new generation before continuing delta replay. Listener address and HTTP server timeout changes require a restart.

```bash
sudo systemctl reload automation-gateway.service
# Or, when restart-only settings changed:
sudo systemctl restart automation-gateway.service

sudo systemctl --no-pager --full status automation-gateway.service
sudo journalctl -u automation-gateway.service -n 100 --no-pager
```

Zabbix push failures are logged and retried without stopping collectors or the HTTPS API. Logs identify only the configured Zabbix endpoint and host; tokens are never logged.

## HTTPS API

All routes use the configured TLS listener, client-address allowlist, and bearer-token authentication. API requests read the cache and never trigger device polling.

- `GET /api/v1/metrics/all` returns one complete cache snapshot and the matching cursor. External consumers use it to bootstrap or resynchronize.
- `GET /api/v1/metrics/delta?since=<cursor>` returns whole cache transactions after that cursor, including `next_cursor` and `has_more`. Cursor generation changes or retention gaps require another full snapshot; malformed or future cursors fail closed.
- `GET /api/v1/status` returns gateway and collector status.
- `GET /api/v1/status/events` streams complete status snapshots with Server-Sent Events.

The metrics endpoints remain available to external integrations when the built-in Zabbix worker is enabled.

## Zabbix details

- [Delta migration and operator procedure](docs/ZABBIX_DELTA_MIGRATION.md)
- [Delta integration architecture and delivery semantics](docs/architecture/zabbix-delta-integration.md)
- [Operator reference](docs/OPERATOR_REFERENCE.md)

The worker persists a cursor only after a complete confirmed `history.push`, preserves original metric timestamps, refreshes the snapshot used for low-level discovery separately, and provides at-least-once delivery.

## Security notes

Collectors remain read-only. Keep certificate keys and tokens outside Git, use narrowly scoped service identities, retain certificate verification, and restrict allowed clients and outbound Zabbix access to required addresses. The systemd unit deliberately keeps serial devices visible for configured Modbus RTU sources; it does not use `PrivateDevices=yes`.
