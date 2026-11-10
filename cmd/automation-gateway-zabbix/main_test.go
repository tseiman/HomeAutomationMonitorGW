package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandAcceptsOnlyConfigFlag(t *testing.T) {
	if err := run([]string{}); err == nil {
		t.Fatal("missing config accepted")
	}
	if err := run([]string{"-gateway-token", "secret"}); err == nil {
		t.Fatal("secret CLI flag accepted")
	}
	if err := run([]string{"-version"}); err != nil {
		t.Fatalf("version flag failed: %v", err)
	}
}

func TestCheckFlagParsesConfigurationWithoutStarting(t *testing.T) {
	err := run([]string{"-config", "/missing/config.yaml", "-check"})
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("check flag was not accepted as configuration validation: %v", err)
	}
}

func TestCheckDoesNotRequireSystemdStateDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"gateway.token", "zabbix.token"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("placeholder-not-a-real-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := fmt.Sprintf("gateway_url: https://gateway.example.invalid\ngateway_token_file: %s\nzabbix_api_url: https://zabbix.example.invalid/api_jsonrpc.php\nzabbix_token_file: %s\nzabbix_host: automation-gateway\ncursor_state_path: %s\n", filepath.Join(dir, "gateway.token"), filepath.Join(dir, "zabbix.token"), filepath.Join(dir, "missing-state", "cursor"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-config", path, "-check"}); err != nil {
		t.Fatalf("pre-start check failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing-state")); !os.IsNotExist(err) {
		t.Fatalf("check created systemd-owned state path: %v", err)
	}
}
