# Passive monitoring integration architecture

## Network direction

```text
NUT / SNMPv3 / Modbus RTU
          │ scheduled read-only collector polls
          ▼
automation-gateway in-memory cache + bounded journal
          ▲
          │ authenticated HTTPS GET
          │
Zabbix HTTP agent or another pull client
```

The gateway only listens and responds. It has no outbound monitoring-system client, remote API credential, callback, sender process, or integration-specific state file. HTTP handlers read the cache and journal; they never initiate or accelerate device polls.

## Snapshot and replay contract

`GET /api/v1/metrics/all` copies the complete source snapshot and its matching opaque cursor under one lock. A cursor-capable client then requests `GET /api/v1/metrics/delta?since=<cursor>` and applies complete transactions in order.

The client owns the cursor:

1. apply the full `sources` snapshot and retain its `cursor`;
2. request a delta with that cursor;
3. apply all transactions and advance only to `next_cursor` after successful local application;
4. while `has_more` is true, immediately request the next page with `next_cursor`;
5. otherwise poll later from the last applied cursor.

A failed or ambiguous request is retried with the same cursor. Reads do not consume journal entries, so multiple readers may start at different points and replay independently. Transactions retain intermediate values, metadata changes, status transitions, removals, discovery changes, and heartbeat records.

An expired cursor returns `410 Gone`. A cursor from a different process or reload generation returns `409 Conflict` with `reset_required: true`. A future cursor returns `409 Conflict`; malformed input returns `400 Bad Request`. Expired and wrong-generation readers discard incremental state and bootstrap again from `/api/v1/metrics/all`.

## Zabbix template

The shipped Zabbix 7.4 template uses one authenticated HTTPS HTTP-agent master item. It fetches `/api/v1/metrics/all`, verifies the certificate peer and hostname, and transforms the versioned `sources` array to the historical source-keyed object used by dependent items and discovery rules. Its bearer value is a Zabbix `SECRET_TEXT` macro sent in the HTTP header. The gateway allowlist must include the actual Zabbix server or proxy source address.

Native Zabbix HTTP items do not retain the gateway's opaque replay cursor in this template. Periodic full snapshots preserve the existing dependent-item, discovery, and dashboard behavior; the delta endpoint remains available to pull clients that implement cursor ownership explicitly.
