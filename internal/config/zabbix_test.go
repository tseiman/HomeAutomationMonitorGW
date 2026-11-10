package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestZabbixDisabledHasDefaultsWithoutReadingToken(t *testing.T) {
	cfg := minimalConfigFile(t, "zabbix:\n  enabled: false\n  token_file: /missing/token\n")
	got, err := Load(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Zabbix.Enabled || got.Secrets.ZabbixToken != "" {
		t.Fatalf("disabled zabbix had side effects: %+v", got.Zabbix)
	}
	if got.Zabbix.PollInterval.Duration != 5*time.Second || got.Zabbix.InventoryInterval.Duration != 15*time.Minute || got.Zabbix.CursorStatePath != "/var/lib/automation-gateway/zabbix-cursor" {
		t.Fatalf("defaults = %+v", got.Zabbix)
	}
}

func TestZabbixEnabledLoadsProtectedTokenAndRejectsUnknownFields(t *testing.T) {
	d := t.TempDir()
	token := filepath.Join(d, "zabbix.token")
	if err := os.WriteFile(token, []byte("zabbix-secret-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := minimalConfigFile(t, "zabbix:\n  enabled: true\n  api_url: https://zabbix.example/api_jsonrpc.php\n  token_file: "+token+"\n  host: gateway\n")
	got, err := Load(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Secrets.ZabbixToken != "zabbix-secret-token" {
		t.Fatal("token not loaded")
	}
	bad := minimalConfigFile(t, "zabbix:\n  enabled: false\n  gateway_url: https://forbidden.example\n")
	if _, err := Load(bad, true); err == nil || !strings.Contains(err.Error(), "gateway_url") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestZabbixEnabledValidatesURLIntervalsAndRequiredFields(t *testing.T) {
	for _, body := range []string{
		"zabbix:\n  enabled: true\n",
		"zabbix:\n  enabled: true\n  api_url: http://zabbix.example/api_jsonrpc.php\n  token_file: /x\n  host: h\n",
		"zabbix:\n  enabled: true\n  api_url: https://zabbix.example/api_jsonrpc.php\n  token_file: /x\n  host: h\n  poll_interval: 4s\n",
	} {
		if _, err := Load(minimalConfigFile(t, body), false); err == nil {
			t.Fatalf("accepted invalid config:\n%s", body)
		}
	}
}

func minimalConfigFile(t *testing.T, extra string) string {
	t.Helper()
	d := t.TempDir()
	cert, key := filepath.Join(d, "server.crt"), filepath.Join(d, "server.key")
	token := filepath.Join(d, "gateway.token")
	if err := os.WriteFile(token, []byte("gateway-token-value-0001"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d, "config.yaml")
	body := "server:\n  listen: 127.0.0.1:8443\n  certificate: " + cert + "\n  private_key: " + key + "\nauthentication:\n  bearer_token_files: [" + token + "]\n  allowed_clients: [127.0.0.1]\nsources: []\n" + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
