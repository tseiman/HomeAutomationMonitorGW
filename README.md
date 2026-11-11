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

Start from [`configs/config.example.yaml`](configs/config.example.yaml). It documents HTTPS, authentication, delta-journal limits, logging, and sources. Unknown YAML fields are rejected. There is no Zabbix configuration section: the gateway needs no Zabbix URL, credential, CA override, cursor file, or persistent integration state; `/metrics/poll` keeps only bounded per-token progress in memory.

Every API client needs both:

- its actual TCP source address or canonical CIDR in `authentication.allowed_clients`;
- a protected token file in `authentication.bearer_token_files`.

Assign one distinct token to each logical polling reader. The token digest identifies that reader's in-memory poll cursor; raw tokens are never returned or logged.

For Zabbix, allow the server or proxy address. Import [`zabbix/template_homeauthmonitorgw.yaml`](zabbix/template_homeauthmonitorgw.yaml), set its gateway URL and dedicated secret token, and keep certificate peer and hostname verification enabled. Exactly one native HTTP-agent item polls `/api/v1/metrics/poll` every 5 seconds. Its changing raw payload is not stored. Instead, `automation.gateway.reachable` derives a constant `1` from every accepted HTTP 200 value and stores an unchanged heartbeat at most once a minute, so reachability needs no second request and remains true even if the body is malformed. The stable `automation.gateway.snapshot` dependent item separately validates and projects the poll envelope for all existing dependents. The poll receives a full snapshot initially, when a new metric identity appears, and at least every `delta.poll_full_interval` (default 10 minutes), with latest-state sparse changes between full responses. Valid empty sparse polls become a stable heartbeat sentinel, while malformed envelopes are discarded by the snapshot. No additional health request is made. Package installation never imports or changes Zabbix.

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
- `GET /api/v1/metrics/poll` provides token-identified full/latest-state sparse polling for native HTTP clients.
- `GET /api/v1/status` and `/api/v1/status/events` expose runtime status.

A cursor-capable client owns its cursor:

1. fetch and apply `/api/v1/metrics/all`;
2. request a delta with the snapshot cursor;
3. apply transactions in order and advance only to `next_cursor` after successful local application;
4. continue immediately while `has_more` is true;
5. poll later from the last applied cursor.

Retry a failed or ambiguous generic delta request with the same cursor. Multiple readers may bootstrap at different cursors and replay independently. On `410 Gone`, or `409 Conflict` with `reset_required: true`, discard incremental state and take a new full snapshot. Native `/metrics/poll` commits token progress only after its response is encoded, completely written, and locally flushed; a local write failure therefore replays the same current-state change or full response. There is still no end-to-end acknowledgement: a transport failure after a successful local flush is ambiguous, intermediate changes may collapse to the latest value, and the periodic full response repairs uncertain state. It is not a lossless history transport and does not preserve original per-change timestamps.

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
