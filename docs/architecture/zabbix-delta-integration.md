# Zabbix delta integration architecture

## Data path

```text
NUT / SNMPv3 / Modbus RTU
          │ fixed gateway polls
          ▼
Gateway cache + bounded transaction journal
          │ HTTPS: /api/v1/metrics/all and /api/v1/metrics/delta
          ▼
automation-gateway-zabbix
          │ HTTPS JSON-RPC: history.push
          ▼
Zabbix trapper items and history
```

The bridge is a network-only consumer. It neither opens device nodes nor changes the gateway polling schedule. It authenticates separately to the gateway and Zabbix API and sends values to pre-created trapper items on one configured Zabbix host.

## Replay and cursor commit order

On first use, after a runtime-generation change, or after an expired cursor, the bridge reads `/api/v1/metrics/all`. It pushes the snapshot item first, pushes the snapshot's metric and source-health values, and only then commits the returned cursor. During normal replay it processes journal transactions in order. For each transaction it:

1. derives generic history keys and preserves the metric or transaction timestamp;
2. completes every `history.push` batch;
3. atomically persists that transaction's cursor under `/var/lib/automation-gateway-zabbix`.

A failed or ambiguous push never advances the cursor. A restart therefore retries the transaction. This is **at-least-once delivery**, not exactly-once delivery: a request may have reached Zabbix even when its response was lost, so retries can submit the same `(host, key, clock, ns, value)` again. Zabbix behavior and retention determine whether a repeated submission is coalesced or stored again; operators must not assume deduplication.

## Snapshot-only discovery

Low-level discovery and dependent inventory items use only the complete `automation.gateway.snapshot` value. The bridge refreshes that snapshot every 15 minutes by default, after first draining replayable deltas, and does not move the replay cursor during the inventory refresh. Delta transactions carry metric and health history, not a partial discovery document.

The mapping is generic. Numeric, boolean, text, and hexadecimal values are selected by schema metadata and stable identities; no device class, source name, OID family, or vendor model is hardcoded into the bridge.

## Retries, gaps, and timestamps

The default bridge poll interval is 5 seconds. The gateway's default metric heartbeat is 5 minutes, and the bridge's default inventory refresh is 15 minutes.

- Transport, authentication, schema, and Zabbix API failures are logged and retried on a later poll without cursor advancement.
- A malformed local cursor fails closed.
- A wrong-generation or pruned gateway cursor triggers a new complete snapshot; an unexplained sequence gap or non-progressing page fails closed rather than skipping data.
- Metric timestamps come from the gateway metric. Source-health records use the transaction capture time. Snapshot inventory uses the full-snapshot capture time.
- The bridge logs recovery once after a failed polling period; it does not emit a success message every 5 seconds.

## Expected load characteristics

The table compares the legacy HTTP/dependent-item path with the delta bridge. Actual payload and write counts depend on source count, metric sizes, heartbeats, failures, and Zabbix preprocessing. CPU impact must be measured on the deployed Zabbix server and database; no proportional CPU reduction is implied.

| Value behavior | Gateway device poll traffic | Legacy HTTP/API network payload | Delta bridge network payload | Legacy Zabbix history rows/writes | Delta bridge history rows/writes | Discovery cadence | Processing effort |
|---|---|---|---|---|---|---|---|
| Constant value | Unchanged; configured collectors still poll, commonly every 5s | Complete master snapshot on every Zabbix interval | Initial value, then gateway heartbeat (default 5m), plus protocol overhead | Dependent-item evaluation each master fetch; storage follows item/history policy | Initial row and heartbeat submissions; exact database writes depend on Zabbix | Complete snapshot only, default 15m | Less repeated JSON transfer and preprocessing is expected; measure Zabbix CPU |
| Frequently changing value | Unchanged; configured collectors still poll, commonly every 5s | Complete master snapshot on every Zabbix interval | Changed transaction records, polled by bridge every 5s | Dependent-item evaluation each master fetch; changing values commonly create history | Changed values commonly create history, potentially near collector cadence | Complete snapshot only, default 15m | Delta parsing and `history.push` batching replace repeated full-snapshot dependent processing; measure Zabbix CPU |

The bridge reduces repeated full-snapshot transfer for constant metrics. It cannot reduce device polling traffic, and frequently changing values still require network transfer and history processing.
