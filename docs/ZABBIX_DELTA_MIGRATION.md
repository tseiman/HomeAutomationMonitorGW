# Migrate Zabbix from HTTP dependent items to the delta bridge

This operator-controlled procedure replaces the legacy Zabbix HTTP master/dependent-item data path with the packaged delta bridge and trapper template. It does not change gateway device polling. The package does not enable or start the service, create credentials, or import a template.

## Prerequisites and backup

- A gateway version exposing `/api/v1/metrics/all` and `/api/v1/metrics/delta`, with replay retention long enough for expected outages.
- HTTPS trust paths from the bridge host to the gateway and Zabbix frontend/API.
- Zabbix 7.4 with `history.push` available.
- A dedicated gateway bearer token and a dedicated Zabbix API token.
- A maintenance window in which the old and new item sets can run in parallel.

Before changing Zabbix, export the linked host, the existing `HomeAuthMonitorGW` template, dashboards, value maps, and any local overrides. Back up `/etc/automation-gateway`, the current Zabbix database by the site's normal procedure, and any existing bridge configuration. Record current latest-data timestamps, unsupported items, trigger state, and the active template linkage for rollback comparison.

## Install and protect files

```bash
sudo apt install automation-gateway-zabbix
sudoedit /etc/automation-gateway-zabbix/config.yaml
```

The package creates `automation-gateway-zabbix`, installs an example configuration, unit, template copy, notices, and binary, and runs `systemctl daemon-reload`. It deliberately does not enable or start the unit.

Use these ownership and mode contracts:

- `/etc/automation-gateway-zabbix`: `root:automation-gateway-zabbix` `0750`;
- `config.yaml`, gateway token, Zabbix token, and private CA files: regular, singly linked files, normally `root:automation-gateway-zabbix` `0640`; service-owned `0600` credentials are also accepted;
- `/var/lib/automation-gateway-zabbix`: systemd-managed `StateDirectory`, service-owned `0700`; do not place credentials there.

The YAML must contain paths only, never token values. Keep the example `.invalid` endpoints and placeholder host out of production configuration.

## Zabbix identity and network permissions

Create a dedicated Zabbix service user and API token. Assign the narrowest API role that permits `history.push`, plus write permission to the single target host used by `zabbix_host`. Do not reuse an administrator token. Confirm that trapper items on that host accept the exact generic keys from the supplied template.

`history.push` is submitted to the Zabbix frontend API. For every affected trapper item, **Allowed hosts must include the actual Zabbix frontend IP address**, because the history submission originates there—not the bridge host and not the gateway. In an HA or load-balanced frontend deployment, allow every possible frontend source IP narrowly.

Allow the bridge host outbound HTTPS only to the configured gateway and Zabbix API endpoints, plus required DNS/NTP. The bridge requires no serial, USB, GPIO, raw-socket, or Linux capability access.

## Import and stage the trapper template

The package installs a review copy at:

```text
/usr/share/automation-gateway-zabbix/template_homeauthmonitorgw.yaml
```

Template import remains an operator action. Review/export first, then import through the Zabbix UI and link it initially to a staging host or to the production host with duplicate triggers/actions disabled. Do not delete the old HTTP master item until comparison is complete.

The new discovery path is snapshot-only and normally refreshes every 15 minutes. Metric deltas are checked every 5 seconds, while unchanged metrics are repeated by the gateway heartbeat every 5 minutes by default. The old timestamp item prototypes are deprecated: retain them only during comparison, then remove or disable them after all dependent dashboards, triggers, and exports have migrated.

## Check, start, and verify

Validate with the installed service identity. This checks strict YAML, endpoint syntax, and credential file type/ownership/mode without entering the poll loop or creating the systemd-owned state directory. The first service start additionally loads configured CA files and opens the cursor state:

```bash
sudo -u automation-gateway-zabbix \
  /usr/sbin/automation-gateway-zabbix \
  -config /etc/automation-gateway-zabbix/config.yaml \
  -check
```

Only after that succeeds:

```bash
sudo systemctl enable --now automation-gateway-zabbix.service
sudo systemctl --no-pager --full status automation-gateway-zabbix.service
sudo journalctl -u automation-gateway-zabbix.service -n 100 --no-pager
```

Verify all of the following before cutover:

1. the startup journal identifies the configured gateway URL, Zabbix API URL, and host but contains no tokens;
2. a cursor appears below `/var/lib/automation-gateway-zabbix` and advances after successful pushes;
3. trapper items receive current values with source timestamps;
4. snapshot-backed discovery appears after the initial push and remains stable across a 15-minute refresh;
5. intentionally constant values reappear near the 5-minute heartbeat, while changing values follow collector transactions;
6. source health, triggers, dashboards, and history match the old path through an agreed comparison period;
7. failure and recovery are visible in the bridge journal without a success line every 5 seconds.

CPU, database writes, and frontend/server load must be measured during the staged comparison. Do not infer a proportional Zabbix CPU reduction from reduced full-snapshot payloads.

## Cut over and rollback

After acceptance, disable the old HTTP master item and its dependent triggers/actions before unlinking or deleting anything. Observe at least one inventory refresh and one metric heartbeat. Keep the exported old template and recorded host state until the rollback window closes.

Rollback is staged and operator-controlled:

1. re-enable the old HTTP master/dependent path while leaving its prior configuration unchanged;
2. disable actions from the new trapper items, then stop and disable `automation-gateway-zabbix.service`;
3. relink or restore the exported old template only if ordinary re-enable is insufficient;
4. verify old latest-data timestamps and triggers before removing the new template linkage;
5. preserve the bridge cursor and configuration until the incident is understood; uninstall only after evidence is collected.

Do not run both paths with duplicate notification actions enabled. Removing the package stops the bridge through `prerm`; upgrades do not stop, start, enable, or restart it. No production, device, Zabbix, or network commands were executed while preparing this guide.
