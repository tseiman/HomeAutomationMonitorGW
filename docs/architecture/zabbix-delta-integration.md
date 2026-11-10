# Zabbix delta integration architecture

## Data path

```text
NUT / SNMPv3 / Modbus RTU
          │ read-only collector polls
          ▼
automation-gateway cache + bounded transaction journal
          │ direct in-process Store.Full / Store.Delta adapter
          ▼
optional automation-gateway Zabbix worker
          │ HTTPS JSON-RPC: history.push
          ▼
Zabbix trapper items and history
```

The worker is part of the standard process, package, account, configuration, and systemd lifecycle. It never polls a device through HTTP and needs no gateway URL or gateway token. The authenticated `/api/v1/metrics/all` and `/api/v1/metrics/delta?since=<cursor>` routes remain available to external consumers.

## Lifecycle

When disabled, the gateway does not load a Zabbix token, validate/open Zabbix-specific CA or cursor state, create a worker, or make Zabbix requests. When enabled, candidate configuration and protected files are validated before the active runtime is stopped. The worker starts under the same generation context as collectors. Reload and shutdown cancel it, wait for it, then close its cursor lock/resources.

Each cache generation has a new cursor identity. Therefore a newly started replacement worker naturally bootstraps from the active store. Zabbix polling errors are logged and retried without terminating collectors or the HTTPS server.

## Replay and cursor commit order

On first use, after a runtime-generation change, or after an expired cursor, the worker calls `Store.Full`. It pushes the snapshot item first, pushes snapshot metric and source-health values, and commits the returned cursor only after all pushes succeed. During normal replay it calls `Store.Delta` and processes journal transactions in order:

1. derive generic history keys while preserving metric/transaction timestamps;
2. complete every `history.push` batch;
3. atomically persist that transaction's cursor at the configured path (default `/var/lib/automation-gateway/zabbix-cursor`).

Failed or ambiguous pushes never advance the cursor, so restart/retry provides **at-least-once delivery**, not exactly-once delivery. Zabbix may receive a duplicate `(host, key, clock, ns, value)` after an ambiguous response; operators must not assume deduplication.

Wrong-generation and expired cursors request bootstrap/resynchronization. Malformed and future cursors fail closed. Non-progressing pages and unexplained schema/sequence errors also fail closed.

## Snapshot-only discovery

Low-level discovery and dependent inventory items use only the complete `automation.gateway.snapshot`. The worker refreshes that snapshot every 15 minutes by default after draining replayable deltas, without moving the replay cursor. Delta transactions carry metric and health history, not partial discovery documents.

The mapping is generic. Numeric, boolean, text, and hexadecimal values are selected by schema metadata and stable identities; no device class, source name, OID family, or vendor model is hardcoded.

## Timing, logs, and load

Defaults are a 5-second delta poll, 15-minute inventory refresh, 10-second request timeout, and the gateway's 5-minute unchanged-value heartbeat. Metric timestamps come from the metric; source-health records use transaction capture time; inventory uses full-snapshot capture time.

Startup, recovery, and shutdown logs identify only the Zabbix endpoint/host. Tokens are excluded, and successful polls do not create five-second log spam.

Delta forwarding reduces repeated full-snapshot transfer for constant metrics. It does not reduce configured device polling, and frequently changing values still require network transfer and Zabbix history processing. Measure frontend, server, and database effects in the target environment.
