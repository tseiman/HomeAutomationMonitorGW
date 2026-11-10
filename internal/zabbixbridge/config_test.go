package zabbixbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	d := t.TempDir()
	for _, name := range []string{"gateway.token", "zabbix.token"} {
		if err := os.WriteFile(filepath.Join(d, name), []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := "gateway_url: https://gateway.example\n" +
		"gateway_token_file: " + filepath.Join(d, "gateway.token") + "\n" +
		"zabbix_api_url: https://zabbix.example/api_jsonrpc.php\n" +
		"zabbix_token_file: " + filepath.Join(d, "zabbix.token") + "\n" +
		"zabbix_host: gateway-host\n" +
		"cursor_state_path: " + filepath.Join(d, "state", "cursor") + "\n" + extra
	path := filepath.Join(d, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaultsAndStrictFields(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != 5*time.Second || cfg.InventoryInterval != 15*time.Minute || cfg.RequestTimeout != 10*time.Second || cfg.BatchSize != 100 || cfg.MaxPages != 100 || cfg.MaxResponseBytes != 8<<20 {
		t.Fatalf("defaults: %+v", cfg)
	}
	for _, extra := range []string{"unknown: true\n", "poll_interval: 4s\n", "poll_interval: 0s\n", "inventory_interval: 4s\n", "gateway_url: http://insecure.example\n"} {
		if _, err := loadConfig(writeConfig(t, extra)); err == nil {
			t.Fatalf("accepted %q", extra)
		}
	}
}

func TestLoadConfigReadsProtectedTokensAndRedactsErrors(t *testing.T) {
	path := writeConfig(t, "")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.gatewayToken == "" || cfg.zabbixToken == "" {
		t.Fatal("tokens not loaded")
	}
	if strings.Contains(cfg.String(), cfg.gatewayToken) || strings.Contains(cfg.String(), cfg.zabbixToken) {
		t.Fatal("config string exposes token")
	}
	if err := os.Chmod(cfg.GatewayTokenFile, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("rejected protected owner/group-readable token: %v", err)
	}
	if err := os.Chmod(cfg.GatewayTokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("accepted world-readable token")
	}
}
