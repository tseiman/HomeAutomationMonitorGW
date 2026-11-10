# HomeAutomationMonitorGW

HomeAutomationMonitorGW is a read-only Linux monitoring gateway. It polls configured NUT, SNMPv3, and generic Modbus RTU sources, caches the latest values, and exposes them through an authenticated HTTPS/JSON API for systems such as Zabbix.

[![Existing architecture view of the SNMPv3 and NUT paths; generic Modbus RTU is documented below](docs/assets/homeautomationmonitorgw-architecture.png)](docs/assets/homeautomationmonitorgw-architecture.svg)

- HTTP requests never trigger device access.
- SNMP uses GET/BulkWalk only; Modbus supports read-only FC01 through FC04.
- Poll failures retain the last-known-good sample and expose its stale state.
- Configuration is strict YAML; protected inputs and permissions are validated.
- The service runs unprivileged and stores no telemetry database.

## Requirements

- Linux on `amd64`, `arm64`, `armv7`/Debian `armhf`, or `386`/Debian `i386`.
- A dedicated unprivileged `automation-gateway` account.
- Read access to configured NUT, SNMPv3, and serial endpoints.
- Membership in `dialout` when Modbus RTU requires access to a serial device.
- Existing TLS certificate, private key, and bearer-token files.

The optional LED client requires a supported Revolution Pi, an `automation-gateway-led` account, and normally membership in `picontrol`.

## Install and update with APT

The signed repository provides the independently installable `automation-gateway` and `automation-gateway-led` packages. Packages preserve existing configuration and credentials and intentionally do not start or restart services.

### Add the repository

```bash
sudo apt update
sudo apt install --yes ca-certificates curl gnupg
sudo install -d -m 0755 /etc/apt/keyrings

curl --fail --silent --show-error --location \
  https://tseiman.github.io/HomeAutomationMonitorGW/apt/repository-key.asc \
  --output /tmp/homeautomationmonitorgw-archive-key.asc

gpg --batch --show-keys --fingerprint \
  /tmp/homeautomationmonitorgw-archive-key.asc
```

Expected fingerprint:

```text
FBB4 E68D ABB4 A25F 6E73  C81D D4EA 266E 8BC8 043F
```

Continue only when the complete fingerprint matches:

```bash
gpg --batch --dearmor \
  < /tmp/homeautomationmonitorgw-archive-key.asc \
  | sudo tee /etc/apt/keyrings/homeautomationmonitorgw.gpg >/dev/null
sudo chmod 0644 /etc/apt/keyrings/homeautomationmonitorgw.gpg
rm -f /tmp/homeautomationmonitorgw-archive-key.asc

ARCH="$(dpkg --print-architecture)"
case "$ARCH" in
  amd64|arm64|armhf|i386) ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

printf '%s\n' \
  "deb [arch=$ARCH signed-by=/etc/apt/keyrings/homeautomationmonitorgw.gpg] https://tseiman.github.io/HomeAutomationMonitorGW/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/homeautomationmonitorgw.list >/dev/null

sudo apt update
```

Do not use `trusted=yes`, global `apt-key`, or `--allow-unauthenticated`.

### Install the gateway

```bash
sudo apt install automation-gateway
sudoedit /etc/automation-gateway/config.yaml
sudo -u automation-gateway /usr/sbin/automation-gateway \
  --config=/etc/automation-gateway/config.yaml \
  --check
sudo systemctl enable --now automation-gateway.service
```

If Modbus RTU uses a device owned by `dialout`:

```bash
sudo usermod -aG dialout automation-gateway
sudo systemctl restart automation-gateway.service
```

### Install the optional LED client

```bash
sudo apt install automation-gateway-led
sudoedit /etc/automation-gateway-led/config.yaml
sudo -u automation-gateway-led /usr/sbin/automation-gateway-led \
  --config=/etc/automation-gateway-led/config.yaml \
  --check
sudo systemctl enable --now automation-gateway-led.service
```

### Update

```bash
sudo apt update
sudo apt install --only-upgrade automation-gateway automation-gateway-led
sudo systemctl restart automation-gateway.service automation-gateway-led.service
```

Omit the LED package and service when it is not installed. Validate configuration before restarting when an update requires configuration changes.

### Migrate an older archive installation

Do not mix archive and package installations. Older archive installations normally use `/usr/local/sbin` and units below `/etc/systemd/system`; packages use `/usr/sbin` and `/lib/systemd/system`.

Before switching, back up `/etc/automation-gateway` and `/etc/automation-gateway-led`, then inspect the active units:

```bash
sudo systemctl show -p FragmentPath,ExecStart automation-gateway.service
sudo systemctl cat automation-gateway.service
sudo systemctl show -p FragmentPath,ExecStart automation-gateway-led.service
```

Stop both services, install the packages, and validate the existing configurations using the `/usr/sbin` binaries. Remove old full units from `/etc/systemd/system` only after confirming they belong to the previous archive installation. Then run `systemctl daemon-reload`, enable the package units, and verify `FragmentPath`, `ExecStart`, and `/proc/$PID/exe` point to `/lib/systemd/system` and `/usr/sbin`.

## Configuration

Start from [`configs/config.example.yaml`](configs/config.example.yaml). Package examples are installed below `/usr/share/doc/automation-gateway/examples/`.

Important paths:

- `/etc/automation-gateway/config.yaml`
- `/etc/automation-gateway/tokens/`
- `/etc/automation-gateway/secrets/`
- site-selected TLS certificate and private-key paths
- `/etc/automation-gateway-led/config.yaml`

Keep secrets in separate files, not directly in YAML. Protected directories should normally be `root:automation-gateway` with mode `0750`; protected files should normally be `root:automation-gateway` with mode `0640`.

Validate every configuration change before reload or restart:

```bash
sudo -u automation-gateway /usr/sbin/automation-gateway \
  --config=/etc/automation-gateway/config.yaml \
  --check
```

### Server and authentication

Configure the HTTPS listen address, certificate, private key, bearer-token files, and allowed client addresses. Every API request requires an allowed TCP peer and `Authorization: Bearer <token>`.

### Polling

Global defaults and per-source overrides control poll interval, timeout, retries, retry delay, and stale threshold. Only a complete successful poll replaces the cache; partial results are never published.

### NUT

Configure the upsd endpoint and exact UPS name. The collector authenticates when credentials are configured and reads `LIST VAR`; it never sends NUT control commands.

### SNMPv3

Only SNMPv3 USM is accepted. Configure the authPriv account, protected passphrase files, and fixed numeric OID allowlist. The collector uses read-only GET/BulkWalk operations. A WAGO metadata example is available at [`configs/wago-750-880-metadata.example.json`](configs/wago-750-880-metadata.example.json).

### Generic Modbus RTU

Use [`configs/config.example.yaml`](configs/config.example.yaml) for the generic schema or [`configs/modbus-sirax-bt72xx.example.yaml`](configs/modbus-sirax-bt72xx.example.yaml) for the SIRAX DC example.

Serial parameters, slave ID, register addresses, function code, data type, byte/word order, scale, and offset are configured in YAML. Addresses are zero-based PDU addresses. Values are published as:

```text
published_value = decoded_raw_value * scale + offset
```

Prefer `/dev/serial/by-id/...`. The USB/RS485 adapter handles transmitter direction; the gateway does not control RTS or GPIO. Only FC01 through FC04 are available.

## Operation

```bash
systemctl status automation-gateway.service
journalctl -u automation-gateway.service -n 100 --no-pager
/usr/sbin/automation-gateway --version
```

Reload supported changes with:

```bash
sudo systemctl reload automation-gateway.service
```

A valid reload atomically replaces configuration and collectors. Listen-address and server-timeout changes require a restart. A failed reload leaves the active runtime unchanged.

## API and Zabbix

The authenticated API exposes cached metrics, discovery data, collector health, runtime status, and status Server-Sent Events. It never polls devices on demand.

Import [`zabbix/template_homeauthmonitorgw.yaml`](zabbix/template_homeauthmonitorgw.yaml), set its URL, secret token, interval, and timeout macros, and keep certificate verification enabled. The Zabbix server or proxy address must be included in `allowed_clients`.

## Optional RevPi LED client

The separate LED client consumes `/api/v1/status/events` and maps connectivity, health, and poll activity to RevPi LED A1 or A2. It never accesses NUT, SNMP, or Modbus devices. See [`clients/revpi-led/README.md`](clients/revpi-led/README.md).

## Alternative archive installation

APT is recommended. For offline or custom deployment, published release archives include checksums and `INSTALL.txt`. Archive installations use `/usr/local/sbin` and `/etc/systemd/system`; do not layer them over package installations.

A local architecture-specific archive can be created with:

```bash
./scripts/package_release.sh arm64
```

## Security

- Keep the gateway on a restricted management network.
- Permit API access only from intended clients.
- Restrict outbound traffic to configured monitoring endpoints and required DNS/NTP services.
- Expose only the required serial device.
- Keep private keys, tokens, and SNMP secrets out of YAML and version control.
- Preserve TLS certificate verification.
- Keep every collector read-only.

## Detailed documentation

- [Operator reference](docs/OPERATOR_REFERENCE.md): source builds, archives, rollback, MIB metadata, API schema, Zabbix widget, and troubleshooting.
- [Project license](LICENSE) and [third-party notices](THIRD_PARTY_LICENSES.md).
