# Migrate Zabbix from HTTP dependent items to built-in delta forwarding

This operator-controlled procedure enables the optional Zabbix worker inside the standard `automation-gateway` process. It replaces the legacy Zabbix HTTP master/dependent-item data path with `history.push` trapper items without changing device polling. There is no second application, package, account, configuration tree, or systemd unit.

## Prerequisites and backup

- `automation-gateway` with `/api/v1/metrics/all` and `/api/v1/metrics/delta` available.
- Zabbix 7.4+ with `history.push`.
- HTTPS trust from the gateway host to the Zabbix frontend/API.
- A dedicated Zabbix API token and a pre-created target host.

Export the linked host, current template, dashboards, value maps, and local overrides. Back up `/etc/automation-gateway` and the Zabbix database by the site's normal procedure. Record latest-data timestamps, unsupported items, triggers, and active template linkage.

## Configure the standard gateway

Install or update only the normal package:

```bash
sudo apt install automation-gateway
sudoedit /etc/automation-gateway/config.yaml
```

Add the optional section shown in `configs/config.example.yaml`:

```yaml
zabbix:
  enabled: true
  api_url: "https://zabbix.example.invalid/api_jsonrpc.php"
  token_file: "/etc/automation-gateway/tokens/zabbix-api"
  host: "automation-gateway"
```

The token must be a protected regular, singly linked file owned by root or `automation-gateway`, mode `0640` or stricter. Keep token values out of YAML. The default cursor path is `/var/lib/automation-gateway/zabbix-cursor`; the standard unit creates `/var/lib/automation-gateway` with mode `0700`.

When `enabled: false`, no Zabbix token, network connection, cursor state, or worker is opened.

## Zabbix identity and permissions

Create a dedicated Zabbix service user/API token. Grant only the API role and target-host write permissions required by `history.push`; do not reuse an administrator token. Confirm the supplied template's trapper items accept the generic keys.

`history.push` is submitted to the Zabbix frontend API. Each affected trapper item's **Allowed hosts** must include the actual frontend source IP. In HA or load-balanced deployments, allow every possible frontend source IP narrowly.

Allow the gateway host outbound HTTPS only to the configured Zabbix API endpoint plus required DNS/NTP. Built-in forwarding does not add device operations; collectors remain read-only.

## Import and stage the template

Review and manually import `zabbix/template_homeauthmonitorgw.yaml` (the package copy is `/usr/share/automation-gateway/template_homeauthmonitorgw.yaml`). Package installation never imports it or changes Zabbix.

Link it first to a staging host, or disable duplicate actions while comparing on production. Snapshot-backed discovery defaults to a 15-minute refresh. Metric deltas default to a 5-second poll; unchanged metrics follow the gateway heartbeat, normally 5 minutes.

## Validate, reload, and verify

```bash
sudo -u automation-gateway /usr/sbin/automation-gateway \
  --config /etc/automation-gateway/config.yaml --check
sudo systemctl reload automation-gateway.service
sudo systemctl --no-pager --full status automation-gateway.service
sudo journalctl -u automation-gateway.service -n 100 --no-pager
```

Validation loads the protected token, validates the HTTPS endpoint/CA and secure cursor path, and completes before the active generation is stopped. Reload cancels and waits for the old collectors and worker, publishes a new cache generation, and bootstraps forwarding from that generation. Use restart instead when listener or HTTP server settings changed.

Verify:

1. logs name the configured Zabbix endpoint and host but contain no token;
2. `/var/lib/automation-gateway/zabbix-cursor` advances only after confirmed pushes;
3. trapper values retain source timestamps;
4. snapshot discovery appears and remains stable across refresh;
5. constant and changing values follow expected heartbeat/delta cadence;
6. source health, triggers, dashboards, and history match the old path;
7. failures and one recovery event are visible without five-second success spam;
8. the HTTPS API, including `/api/v1/metrics/all` and `/api/v1/metrics/delta?since=<cursor>`, remains independently available.

Measure CPU, database writes, and frontend/server load during comparison; reduced repeated snapshots do not imply proportional CPU savings.

## Cutover and rollback

After acceptance, disable old HTTP items and duplicate actions before unlinking anything. Observe at least one inventory refresh and metric heartbeat.

Rollback is configuration-only:

1. re-enable the old HTTP path and its prior template linkage;
2. set `zabbix.enabled: false` in `/etc/automation-gateway/config.yaml`;
3. validate and reload `automation-gateway.service`;
4. verify old latest-data timestamps and triggers;
5. preserve cursor state and logs until the incident is understood.

Do not run duplicate notification actions on both paths. Package updates do not create tokens, import templates, or start/restart services.
