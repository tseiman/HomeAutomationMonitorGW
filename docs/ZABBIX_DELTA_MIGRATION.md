# Zabbix HTTP-pull migration

This migration restores the supported network direction: Zabbix initiates authenticated HTTPS requests to the passive gateway. The gateway never connects to Zabbix and has no Zabbix URL, API credential, forwarding lifecycle, or Zabbix-specific persistent state.

## Before import

1. Export the linked host, current template, dashboards, value maps, macros, and local overrides.
2. Back up the Zabbix database by the site's normal procedure.
3. Install or update the standard `automation-gateway` package only.
4. Configure the gateway API token file under `authentication.bearer_token_files` and add the actual Zabbix server or proxy TCP source address to `authentication.allowed_clients`.
5. Keep gateway TLS certificate verification available to the Zabbix server or proxy.

The gateway configuration deliberately has no top-level `zabbix` section. Strict YAML decoding rejects one.

## Import and configure

Import `zabbix/template_homeauthmonitorgw.yaml` with update semantics. The template preserves its existing items, discovery rules, triggers, value maps, and dashboards. Configure these macros:

- `{$AUTOMATION_GATEWAY_URL}`: the gateway HTTPS origin;
- `{$AUTOMATION_GATEWAY_TOKEN}`: a Zabbix `SECRET_TEXT` macro used only in the `Authorization` header;
- `{$AUTOMATION_GATEWAY_INTERVAL}` and `{$AUTOMATION_GATEWAY_TIMEOUT}`.

The HTTP-agent master fetches `GET /api/v1/metrics/all` with peer and host verification enabled. Its preprocessing validates the versioned snapshot envelope and converts the `sources` array to the source-keyed JSON object consumed by existing dependent items. It needs no inbound-item source policy because Zabbix initiates the HTTPS request.

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
5. TLS peer and hostname verification remain enabled.

Package installation never imports the template or changes Zabbix.

## Generic cursor-capable consumers

The generic delta API is for clients that can retain their own opaque cursor. Such a client bootstraps with `/api/v1/metrics/all`, applies deltas from `/api/v1/metrics/delta?since=<cursor>`, retries failures with the same cursor, follows `has_more` with `next_cursor`, and resynchronizes from a full snapshot after an expired or wrong-generation cursor. The shipped Zabbix template does not claim to retain an opaque cursor; it intentionally uses periodic full HTTP-agent snapshots.
