# HomeAutomationMonitorGW

HomeAutomationMonitorGW is a read-only Linux monitoring gateway. The `automation-gateway` service polls configured NUT, SNMPv3, and Modbus RTU sources on schedules, retains snapshots and a bounded replay journal in memory, and serves authenticated HTTPS/JSON. Monitoring systems initiate requests; the gateway never connects to Zabbix and HTTP requests never poll devices.

## Requirements

- Linux with systemd for packaged service operation.
- Site-owned TLS certificate/key and bearer-token files.
- Access to configured read-only NUT, SNMPv3, or Modbus RTU sources.
- Go from `go.mod` only for source builds; prebuilt packages need no Go toolchain.

## Install

The signed repository provides `automation-gateway` and the separately optional `automation-gateway-led` package. Package installation preserves site configuration and does not start services.

```bash
sudo apt install automation-gateway
sudoedit /etc/automation-gateway/config.yaml
sudo -u automation-gateway /usr/sbin/automation-gateway \
  --config /etc/automation-gateway/config.yaml --check
sudo systemctl enable --now automation-gateway.service
```

For an archive release, verify its adjacent checksum before extraction, then run `sudo ./scripts/install.sh`. Keep TLS keys, collector secrets, and bearer tokens in protected regular files owned by root or `automation-gateway`, mode `0640` or stricter. Do not put secret values in YAML.

## Configure

Start from [`configs/config.example.yaml`](configs/config.example.yaml). It documents HTTPS, authentication, delta-journal limits, logging, and sources. Unknown YAML fields are rejected. There is no Zabbix configuration section: the gateway needs no Zabbix URL, credential, CA override, cursor file, or integration state.

Every API client needs both:

- its actual TCP source address or canonical CIDR in `authentication.allowed_clients`;
- a protected token file in `authentication.bearer_token_files`.

For Zabbix, allow the server or proxy address. Import [`zabbix/template_homeauthmonitorgw.yaml`](zabbix/template_homeauthmonitorgw.yaml), set its gateway URL, secret token, interval, and timeout macros, and keep certificate peer and hostname verification enabled. The HTTP-agent master fetches `/api/v1/metrics/all`; dependent items, discovery, triggers, and dashboards consume its validated source-keyed projection. Package installation never imports the template or changes Zabbix.

## Validate and operate

```bash
sudo -u automation-gateway /usr/sbin/automation-gateway \
  --config /etc/automation-gateway/config.yaml --check
sudo systemctl reload automation-gateway.service
sudo systemctl --no-pager --full status automation-gateway.service
sudo journalctl -u automation-gateway.service -n 100 --no-pager
```

A successful reload atomically publishes a new cache generation. Listener and HTTP timeout changes require a restart. A failed reload leaves the active runtime unchanged.

All API routes enforce TLS, the client-address allowlist, and bearer authentication. They read only the in-memory cache or journal.

- `GET /api/v1/metrics` retains the established source-keyed response.
- `GET /api/v1/metrics/all` returns an atomic complete `sources` snapshot and matching opaque `cursor`.
- `GET /api/v1/metrics/delta?since=<cursor>` replays whole transactions without consuming them globally.
- `GET /api/v1/status` and `/api/v1/status/events` expose runtime status.

A cursor-capable client owns its cursor:

1. fetch and apply `/api/v1/metrics/all`;
2. request a delta with the snapshot cursor;
3. apply transactions in order and advance only to `next_cursor` after successful local application;
4. continue immediately while `has_more` is true;
5. poll later from the last applied cursor.

Retry a failed or ambiguous request with the same cursor. Multiple readers may bootstrap at different cursors and replay independently. On `410 Gone`, or `409 Conflict` with `reset_required: true`, discard incremental state and take a new full snapshot. The shipped Zabbix template intentionally uses full HTTP-agent snapshots; it does not claim that a native HTTP item retains the opaque cursor.

## Update

```bash
sudo apt update
sudo apt install --only-upgrade automation-gateway
sudo -u automation-gateway /usr/sbin/automation-gateway \
  --config /etc/automation-gateway/config.yaml --check
sudo systemctl restart automation-gateway.service
```

Update and restart `automation-gateway-led` separately only when it is installed. Do not mix archive and Debian-package installations.

## More information

- [Operator reference](docs/OPERATOR_REFERENCE.md)
- [Zabbix HTTP-pull migration](docs/ZABBIX_DELTA_MIGRATION.md)
- [Passive integration architecture](docs/architecture/zabbix-delta-integration.md)
- [Delta journal ADR](docs/architecture/delta-api.md)
- [Optional RevPi LED client](clients/revpi-led/README.md)
