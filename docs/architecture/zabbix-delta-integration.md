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

## Token-identified native polling

`GET /api/v1/metrics/poll` is the stateful adapter for an ordinary native HTTP poller that cannot retain an opaque cursor. Authentication still uses the existing bearer-token list. After constant-time authentication, only the token's SHA-256 digest identifies the reader; the raw token is never returned or logged. Assign one distinct token per logical poll reader. State is concurrency-safe, bounded to the currently configured token digests, memory-only, and removed when a token leaves the active policy.

The first request, a new token, process start, successful reload/cache-generation mismatch, expired cursor, discovery change, metric removal, newly observed source-plus-stable-metric identity, or `delta.poll_full_interval` (default `10m`) returns a full response. Metric removal uses reason `metric_removed`; a new identity uses `metric_added`. Both let full-only discovery and composite items reconcile inventory immediately. The known-identity baseline is built only from a successfully written and flushed full response and resets with token state or generation changes.

```json
{"schema_version":1,"mode":"full","reason":"first_request","generation":"…","cursor":"…","captured_at":"…","sources":[...]}
```

Other requests return `mode: "sparse"`. `sources` contains the latest status and a latest-record-per-metric array of upserts for changed sources; it never contains a hidden full-state copy. Missing sources or metrics mean unchanged, never removed. Intermediate changes between HTTP polls intentionally collapse to the latest record.

The gateway snapshots one token's state under its lock, releases the lock, and computes a prospective response. Only after JSON encoding, a complete local write, and local flush succeed does it reacquire the token lock and commit cursor, full timer, and known-metric baseline if the state version still matches. Failed encoding/write/flush leaves all progress unchanged. A slow writer holds no global or token lock; concurrent requests for the same token can duplicate safely, while different tokens remain independent. Native HTTP still has no application acknowledgement, so delivery after a successful local flush is ambiguous. This endpoint provides eventual/latest-state delivery, not lossless intermediate history or original per-change timestamps; periodic full responses repair uncertain current state.

## Zabbix template

The shipped Zabbix 7.4 template makes exactly one authenticated HTTPS request at the default `5s` interval. Raw HTTP-agent item `automation.gateway.poll.raw` fetches `/api/v1/metrics/poll`, verifies the certificate peer and hostname, and accepts status 200 without body preprocessing. Raw history and trends are disabled, so changing payloads are not stored. Dependent item `automation.gateway.reachable` unconditionally returns numeric `1` for every accepted raw value without parsing the body, then stores unchanged heartbeats at most once a minute. The existing `nodata(...,5m)` reachability trigger monitors this lightweight item, so malformed HTTP 200 bodies still prove reachability without another request.

Stable item `automation.gateway.snapshot` is dependent on the raw item. It validates and projects full or sparse envelopes for every existing dependent item. A valid empty sparse response becomes `{"__poll_mode":"empty"}` and is refreshed by `DISCARD_UNCHANGED_HEARTBEAT` every minute; malformed envelopes are discarded. The separate snapshot `nodata(...,5m)` trigger therefore detects absence of valid data without another network request. Every downstream dependent and discovery path discards the sentinel. Missing sparse metric paths use `DISCARD_VALUE`, so they do not become zero, unsupported, or deleted. Ordinary metric and source-status dependents consume matching sparse paths. Composite and full-state-derived preprocessing, including discovery, accepts only full responses and discards sparse input. Per-metric timestamp prototypes remain `discover: NO` and `status: DISABLED`.

The legacy `/metrics`, `/metrics/all`, and `/metrics/delta` routes remain stateless and unchanged. Cursor-capable clients should continue using full plus generic delta replay when they need ordered retained transactions.
