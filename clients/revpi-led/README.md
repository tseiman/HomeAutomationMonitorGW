# Optional RevPi status LED client

`automation-gateway-led` is an optional Linux systemd service for a Revolution Pi. It consumes HomeAutomationMonitorGW's authenticated SSE endpoint and displays the gateway state on exactly one freely programmable system LED. It is deliberately kept in this directory and is not installed, enabled, or started by the gateway installer.

## State mapping

The selected LED (`A1` or `A2`) uses this priority:

1. **Red** — the HTTPS/SSE connection is unavailable, closes, or sends no bytes before `stream_timeout`.
2. **Orange** — the gateway is reachable and reports collector health code `1` (degraded) or `2` (failed).
3. **Green** — at least one collector is active.
4. **Off** — the gateway is reachable, healthy, and idle.

Errors and disconnects override green immediately. Collector polls can be shorter than a visible flash, so green remains on for at least `activity_hold` after activity begins. The default is `750ms`; a later active event extends the hold. If the gateway coalesces a complete short poll before the client reads both edges, a changed `activity.poll_sequence` still creates the green pulse. The client preserves the two bits belonging to the unselected LED.

## Hardware access

The service uses the process-image variable `RevPiLED` through [`github.com/mezzato/revpi/pkg/gopicontrol`](https://github.com/mezzato/revpi), pinned to `v1.0.0`. The library opens `/dev/piControl0` and is described by its author as experimental. Its compatibility with the concrete RevPi Core 3 image must therefore be checked before enabling the service.

The service runs as the unprivileged user `automation-gateway-led`. Access to `/dev/piControl0` is granted through the RevPi `picontrol` group and restricted in systemd with `DevicePolicy=closed` and `DeviceAllow=/dev/piControl0 rw`.

Verify the LED variable manually on the target first:

```bash
getent group picontrol
ls -l /dev/piControl0
piTest -r RevPiLED
piTest -w RevPiLED,1   # A1 green
piTest -w RevPiLED,0   # both programmable LEDs off
```

The source ZIP supplied for this component documents the Core 3 bit allocation as A1 green/red = bits 0/1 and A2 green/red = bits 2/3. The implementation uses that allocation and verifies that `RevPiLED` is an 8-bit variable.

## Gateway prerequisites

The Gateway must expose the already integrated endpoint:

```text
GET /api/v1/status/events
```

It must use HTTPS. Add the RevPi address to the gateway's `allowed_clients` and configure a dedicated bearer token there. After the optional client installer has created its local service identity, provision the same token on the RevPi without exposing its value in Git or logs:

```bash
openssl rand -hex 32 | sudo install -o root -g automation-gateway-led -m 0640 /dev/stdin /etc/automation-gateway-led/token
```

Use the generated value for the corresponding gateway bearer-token file as well. With the gateway's default 15-second SSE heartbeat, the client default `stream_timeout: "45s"` tolerates two missed heartbeat intervals before showing red and reconnecting.

## Build from a clone

Requirements:

- Go 1.23 or newer;
- Linux for native builds;
- network access once for checksummed Go module download;
- a RevPi Core 3 only for runtime hardware verification, not for compilation.

```bash
git clone https://github.com/tseiman/HomeAutomationMonitorGW.git
cd HomeAutomationMonitorGW
go mod download
CGO_ENABLED=0 go build -trimpath -o /tmp/automation-gateway-led ./clients/revpi-led
/tmp/automation-gateway-led --version
```

Cross-compile for the original ARM-based Core 3 as ARMv6, matching the upstream library's documented target:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 \
  go build -trimpath -o /tmp/automation-gateway-led-armv6 ./clients/revpi-led
```

ARMv7 also builds and is included in the repository's existing `armv7` release target:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
  go build -trimpath -o /tmp/automation-gateway-led-armv7 ./clients/revpi-led
```

## Install from a clone

Installation is intentionally explicit and optional:

```bash
sudo ./clients/revpi-led/install.sh --binary=/tmp/automation-gateway-led
sudoedit /etc/automation-gateway-led/config.yaml
```

Provision `/etc/automation-gateway-led/token` as shown above. For a private CA, install its public PEM certificate and set `ca_file`; TLS verification is never disabled.

Validate without opening `/dev/piControl0`:

```bash
sudo -u automation-gateway-led \
  /usr/local/sbin/automation-gateway-led \
  --config /etc/automation-gateway-led/config.yaml \
  --check
```

After the `piTest` hardware check, explicitly enable the service:

```bash
sudo ./clients/revpi-led/install.sh \
  --binary=/tmp/automation-gateway-led \
  --start
systemctl status automation-gateway-led.service
journalctl -u automation-gateway-led.service -n 50 --no-pager
```

The installer preserves an existing configuration and token. Without `--start`, a previously inactive service remains inactive.

## Install from a release archive

The project release script creates a separate optional archive and checksum for the LED client. It is not placed inside the normal gateway archive.

```bash
sha256sum -c automation-gateway-led-VERSION-linux-armv7.tar.gz.sha256
INSTALL_DIR="$(mktemp -d /tmp/automation-gateway-led-install.XXXXXX)"
tar -xzf automation-gateway-led-VERSION-linux-armv7.tar.gz -C "$INSTALL_DIR"
cd "$INSTALL_DIR"
sudo ./install.sh --binary="$INSTALL_DIR/automation-gateway-led-linux-armv7"
```

Configure and start it only after validating the concrete RevPi hardware as described above.

## Configuration

See [`config.example.yaml`](config.example.yaml). Important fields:

- `gateway_url`: exact HTTPS URL ending in `/api/v1/status/events`;
- `token_file`: absolute path with no access for “other” users;
- `ca_file`: optional private CA PEM; system roots remain enabled;
- `led`: `A1` or `A2`;
- `activity_hold`: minimum visible green duration, default `750ms`;
- `stream_timeout`: no-byte timeout, default `45s` and required to exceed 15 seconds;
- `reconnect_min` / `reconnect_max`: exponential reconnect range.

Bearer tokens are never written to logs. URL userinfo, plain HTTP, unknown YAML fields, unsafe token modes, malformed status frames, and invalid health codes are rejected.

## Removal

```bash
sudo systemctl disable --now automation-gateway-led.service
sudo rm /etc/systemd/system/automation-gateway-led.service
sudo rm /usr/local/sbin/automation-gateway-led
sudo systemctl daemon-reload
```

Site-owned files under `/etc/automation-gateway-led` are intentionally not removed automatically.
