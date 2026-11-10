package scripts

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentationHasNoSeparateZabbixApplicationInstructions(t *testing.T) {
	root := projectRoot(t)
	var paths []string
	for _, name := range []string{"README.md", "docs"} {
		base := filepath.Join(root, name)
		info, err := os.Stat(base)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			paths = append(paths, base)
			continue
		}
		_ = filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
				paths = append(paths, path)
			}
			return err
		})
	}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasPrefix(entry.Name(), "README") && strings.HasSuffix(entry.Name(), ".md") && path != filepath.Join(root, "README.md") {
			paths = append(paths, path)
		}
		return err
	})
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, forbidden := range []string{"automation-gateway-zabbix", "systemctl enable --now automation-gateway-zabbix", "apt install automation-gateway-zabbix"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains stale separate-install surface %q", path, forbidden)
			}
		}
	}
	main, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"optional", "built into the standard process", "/api/v1/metrics/all", "/api/v1/metrics/delta?since=<cursor>", "docs/ZABBIX_DELTA_MIGRATION.md", "docs/architecture/zabbix-delta-integration.md"} {
		if !strings.Contains(string(main), required) {
			t.Errorf("main README lacks %q", required)
		}
	}
}
