package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGatewayHasNoOutboundZabbixSurface prevents reintroducing the rejected push architecture.
func TestGatewayHasNoOutboundZabbixSurface(t *testing.T) {
	root := projectRoot(t)
	output, err := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Fatal(err)
	}
	patterns := []string{
		"history[.]" + "push",
		"zabbix_api_(url|token|ca)",
		"internal/" + "zabbixbridge",
		"automation-gateway-" + "zabbix",
		"zabbix_" + "cursor",
		"zabbix[ _-](worker|state)",
		"trap" + "per",
		"allowed[ _]hosts",
		"api_" + "jsonrpc",
	}
	forbidden := regexp.MustCompile("(?i)(" + strings.Join(patterns, "|") + ")")
	for _, relative := range strings.Fields(string(output)) {
		if strings.HasPrefix(relative, "docs/assets/") || strings.HasPrefix(relative, ".git/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, relative))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if match := forbidden.Find(data); match != nil {
			t.Errorf("%s contains forbidden outbound integration surface %q", relative, match)
		}
	}
}
