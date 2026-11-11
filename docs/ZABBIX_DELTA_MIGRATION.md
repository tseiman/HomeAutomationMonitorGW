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
- `{$AUTOMATION_GATEWAY_INTERVAL}`: the `/metrics/poll` cadence, default `5s`;
- `{$AUTOMATION_GATEWAY_TIMEOUT}`: the HTTPS request timeout.

Exactly one HTTP-agent item, new raw master `automation.gateway.poll.raw`, fetches `GET /api/v1/metrics/poll` with peer and host verification enabled at the default `5s` interval. Raw history and trends are disabled, so its changing payload is never written to history. Lightweight dependent item `automation.gateway.reachable` unconditionally converts every accepted raw value to numeric `1`, without parsing the body, and uses a one-minute discard-unchanged heartbeat with 7-day history and 30-day trends. The existing reachability trigger UUID monitors `nodata(.../automation.gateway.reachable,5m)`. A malformed HTTP 200 therefore proves reachability without storing its body or making a second request.

The existing stable `automation.gateway.snapshot` item keeps its UUID, key, one-day history, and downstream identity, but is now dependent on the raw master. Its JavaScript validates and projects the envelope, and its one-minute discard-unchanged heartbeat refreshes a valid stable value. A valid empty sparse envelope becomes `{"__poll_mode":"empty"}`; malformed envelopes are discarded. The new snapshot `nodata(...,5m)` trigger therefore reports absence of valid poll data independently from HTTP reachability. Every existing metric, source, and discovery dependent remains mastered by the snapshot and discards the empty sentinel. Composite/full-state-derived items and discovery run only on full responses. Metric removal and a newly observed source-plus-stable-metric identity force full responses with reasons `metric_removed` and `metric_added`, respectively, so discovery reconciles immediately. No additional network request is made. Existing deployed item keys, UUIDs, units, scaling, history, other triggers, and dashboard references remain stable; the raw and reachability items plus the data-validity trigger implement the split. Per-metric timestamp prototypes remain not discovered and disabled.

## Validate

```bash
sudo -u automation-gateway /usr/sbin/automation-gateway \
  --config /etc/automation-gateway/config.yaml --check
```

Then verify from Zabbix that:

1. the raw poll master is supported and receives HTTP 200, while the dependent snapshot accepts valid envelopes;
2. source discovery and dependent items update;
3. dashboards and triggers retain their existing item keys;
4. the gateway allowlist contains only intended client addresses;
5. TLS peer and hostname verification remain enabled;
6. quiet valid polling refreshes the stable empty sentinel on its one-minute heartbeat without satisfying the valid-data no-data trigger;
7. changing one existing metric updates only that metric, while adding a new metric produces an immediate `metric_added` full response and refreshed discovery.

Package installation never imports the template or changes Zabbix.

## Generic cursor-capable consumers

The generic delta API is for clients that can retain their own opaque cursor. Such a client bootstraps with `/api/v1/metrics/all`, applies deltas from `/api/v1/metrics/delta?since=<cursor>`, retries failures with the same cursor, follows `has_more` with `next_cursor`, and resynchronizes from a full snapshot after an expired or wrong-generation cursor.

Native `/metrics/poll` computes progress without committing it, releases the token lock, encodes/writes/locally flushes the response, and then commits with a per-token version check. Encoding, write, or flush failure leaves cursor, full timer, and known-metric baseline unchanged; concurrent same-token requests may safely duplicate a response but cannot skip progress. This is only a local delivery boundary. Zabbix supplies no end-to-end acknowledgement, so a failure after successful local flush remains ambiguous. The adapter intentionally collapses intermediate records to latest metric state, and the periodic full response restores current state. Do not describe this path as lossless history or as preserving every original history timestamp.
