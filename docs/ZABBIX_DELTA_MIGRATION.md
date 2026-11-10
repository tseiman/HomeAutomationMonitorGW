# Zabbix HTTP-pull migration

This migration restores the supported network direction: Zabbix initiates authenticated HTTPS requests to the passive gateway. The gateway never connects to Zabbix and has no Zabbix URL, API credential, forwarding lifecycle, or Zabbix-specific persistent state.

## Before import

1. Export the linked host, current template, dashboards, value maps, macros, and local overrides.
2. Back up the Zabbix database by the site's normal procedure.
3. Install or update the standard `automation-gateway` package only.
4. Configure one dedicated Zabbix poll-reader token file under `authentication.bearer_token_files` and add the actual Zabbix server or proxy TCP source address to `authentication.allowed_clients`. Do not share this token with another logical poll reader.
5. Keep gateway TLS certificate verification available to the Zabbix server or proxy.

The gateway configuration deliberately has no top-level `zabbix` section. Strict YAML decoding rejects one.

## Import and configure

Import `zabbix/template_homeauthmonitorgw.yaml` with update semantics. The template preserves its existing items, discovery rules, triggers, value maps, and dashboards. Configure these macros:

- `{$AUTOMATION_GATEWAY_URL}`: the gateway HTTPS origin;
- `{$AUTOMATION_GATEWAY_TOKEN}`: a Zabbix `SECRET_TEXT` macro used only in the `Authorization` header;
- `{$AUTOMATION_GATEWAY_INTERVAL}` and `{$AUTOMATION_GATEWAY_TIMEOUT}`.

The HTTP-agent master fetches `GET /api/v1/metrics/poll` with peer and host verification enabled. The default interval is `5s`; the gateway's generic `delta.poll_full_interval` defaults to `10m`. Sparse envelopes contain only changed source status and metric upserts, never a full-state copy. Preprocessing discards empty sparse responses and missing sparse paths; composite/full-state-derived items and discovery run only on full responses. A metric removal forces a full response with reason `metric_removed` so discovery can reconcile inventory. Existing item keys, UUIDs, units, scaling, history, triggers, and dashboard references remain stable. Per-metric timestamp prototypes remain not discovered and disabled.

## Validate

```bash
sudo -u automation-gateway /usr/sbin/automation-gateway \
  --config /etc/automation-gateway/config.yaml --check
```

Then verify from Zabbix that:

1. the master item is supported and receives HTTP 200;
2. source discovery and dependent items update;
3. dashboards and triggers retain their existing item keys;
4. the gateway allowlist contains only intended client addresses;
5. TLS peer and hostname verification remain enabled;
6. an unchanged sparse poll writes no master value, and changing one metric updates that metric without rewriting unrelated ordinary metric prototypes;
7. discovery remains present through sparse polls and refreshes on the next forced full response.

Package installation never imports the template or changes Zabbix.

## Generic cursor-capable consumers

The generic delta API is for clients that can retain their own opaque cursor. Such a client bootstraps with `/api/v1/metrics/all`, applies deltas from `/api/v1/metrics/delta?since=<cursor>`, retries failures with the same cursor, follows `has_more` with `next_cursor`, and resynchronizes from a full snapshot after an expired or wrong-generation cursor.

Native `/metrics/poll` advances its in-memory token cursor when producing a response because Zabbix supplies no acknowledgement. It intentionally collapses intermediate records to the latest metric state. A lost or ambiguous HTTP response is therefore not replayed losslessly; the periodic full response restores current state. Do not describe this path as lossless history or as preserving every original history timestamp.
