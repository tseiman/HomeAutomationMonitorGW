# ADR: In-memory generic metric delta journal

## Status

Accepted for the gateway milestone.

## Context

Polling is independent of HTTP and existing API response shapes must remain compatible. Incremental readers need every observed value transition, including a short-lived peak and return between reads, without a database or protocol-specific API. Readers may run concurrently, retry requests, lag temporarily, or reconnect after a process restart or configuration reload.

## Decision

The cache store owns the current source snapshots and an in-memory journal protected by the same mutex. A completed poll updates the source snapshot and appends one immutable transaction while holding that lock. The full-snapshot route copies all sources and captures the journal cursor under the same lock, which closes the snapshot-to-delta race.

Each metric carries an internal protocol-stable identity that is omitted from legacy metric JSON:

- SNMP: normalized numeric OID;
- NUT: variable name;
- Modbus RTU: function code, zero-based PDU address, and width.

Delta records carry that identity separately. Successful polls compare exact typed values and all metric metadata while ignoring only the timestamp. New, changed, heartbeat-due, and removed metrics become records; a removal is an explicit tombstone. Every transaction contains the complete source status. Failed polls append status only and retain last-known-good values.

Cursors are `v1.<generation>.<sequence>`. The 128-bit generation is randomly created for every store, including reload-created stores. Sequence numbers identify poll transactions, not readers. Replay is therefore stateless: multiple readers and repeated requests do not consume data. Responses are capped at 1,000 whole transactions and advertise `has_more` and the current `head_cursor`.

The journal is bounded by age, transaction count, and approximate encoded bytes. Pruning removes only whole transactions. The newest transaction survives count or byte pressure even when it alone exceeds the configured byte budget, preventing a successful snapshot update from silently lacking a journal record. Age expiry may remove all transactions. Readers whose cursor predates retained history must reset from a full snapshot.

## Consequences

- HTTP handlers remain cache-only and cannot cause device traffic.
- The established `/api/v1/metrics` and per-source JSON shapes do not expose the new internal identity.
- Restart and reload invalidate cursors deliberately; readers receive an explicit reset response.
- Replay is bounded and not durable across restarts. Consumers needing durable history must persist applied data externally.
- Memory use is predictable apart from one newest oversized transaction, which is retained explicitly rather than silently discarded.
