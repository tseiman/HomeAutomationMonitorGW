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
	for _, required := range []string{"User=automation-gateway", "Group=automation-gateway", "NoNewPrivileges=yes", "StateDirectory=automation-gateway", "StateDirectoryMode=0700"} {
		if !strings.Contains(unit, required) {
			t.Fatalf("unit lacks %q", required)
		}
	}
	if strings.Contains(unit, "PrivateDevices=yes") || strings.Contains(unit, "User=root") {
		t.Fatalf("unit hides configured serial devices or runs as root:\n%s", unit)
	}
}
