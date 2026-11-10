package systemd

import (
	"os"
	"strings"
	"testing"
)

func TestGatewayUnitKeepsSerialDevicesVisibleWithoutRoot(t *testing.T) {
	data, err := os.ReadFile("automation-gateway.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(data)
	for _, required := range []string{"User=automation-gateway", "Group=automation-gateway", "NoNewPrivileges=yes"} {
		if !strings.Contains(unit, required) {
			t.Fatalf("unit lacks %q", required)
		}
	}
	if strings.Contains(unit, "PrivateDevices=yes") || strings.Contains(unit, "User=root") {
		t.Fatalf("unit hides configured serial devices or runs as root:\n%s", unit)
	}
}

func TestZabbixBridgeUnitIsNetworkOnlyAndHardened(t *testing.T) {
	data, err := os.ReadFile("automation-gateway-zabbix.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(data)
	for _, required := range []string{
		"User=automation-gateway-zabbix", "Group=automation-gateway-zabbix",
		"ExecStart=/usr/sbin/automation-gateway-zabbix -config /etc/automation-gateway-zabbix/config.yaml",
		"ProtectSystem=strict", "ProtectHome=yes", "NoNewPrivileges=yes", "PrivateDevices=yes",
		"CapabilityBoundingSet=", "AmbientCapabilities=", "RestrictAddressFamilies=AF_INET AF_INET6",
		"StateDirectory=automation-gateway-zabbix", "StateDirectoryMode=0700",
		"StandardOutput=journal", "StandardError=journal", "Restart=on-failure",
	} {
		if !strings.Contains(unit, required) {
			t.Fatalf("bridge unit lacks %q", required)
		}
	}
	for _, forbidden := range []string{"DeviceAllow=", "SupplementaryGroups=", "User=root"} {
		if strings.Contains(unit, forbidden) {
			t.Fatalf("bridge unit contains forbidden device/privilege setting %q", forbidden)
		}
	}
}
