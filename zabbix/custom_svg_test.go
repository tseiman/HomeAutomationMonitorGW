// File: zabbix/custom_svg_test.go
// Focused tests for the persistent custom SVG asset feature:
//   - JSON map parsing and schema validation (PHP, skipped without php CLI)
//   - Path safety / traversal rejection (PHP, skipped without php CLI)
//   - Fallback tooltip hint content (PHP, skipped without php CLI)
//   - Installer persistence: custom data is never overwritten (Go + bash)

package zabbix

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// phpSkip returns the php binary path or skips the test if php is not installed.
func phpSkip(t *testing.T) string {
	t.Helper()
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not installed — skipping PHP-based test")
	}
	return php
}

// runPhp runs a PHP snippet and returns the trimmed stdout.
func runPhp(t *testing.T, php, snippet string) string {
	t.Helper()
	out, err := exec.Command(php, "-r", snippet).CombinedOutput()
	if err != nil {
		t.Fatalf("php -r: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestCustomMapSchemaValidation checks that the JSON loading/validation logic accepts
// well-formed entries and rejects malformed keys, bad filenames, and invalid entry objects.
func TestCustomMapSchemaValidation(t *testing.T) {
	php := phpSkip(t)

	// PHP inline logic that mirrors the key/filename/entry validation in widget.view.php.
	phpLogic := `
// Controller key: starts with digit, alphanumeric/slash/hyphen.
$validCtrlKey = static fn(string $k): bool =>
    (bool) preg_match('/^[0-9][0-9a-zA-Z\/\-]*$/', $k);
// v2.0: module keys are article names — same regex as controller keys.
$validModKey = static fn(string $k): bool =>
    (bool) preg_match('/^[0-9][0-9a-zA-Z\/\-]*$/', $k);
// Valid image filename: basename only, alphanumeric/underscore/hyphen, .svg suffix.
$validFilename = static fn(string $f): bool =>
    (bool) preg_match('/^[a-zA-Z0-9][a-zA-Z0-9_\-]*\.svg$/', $f);
// Entry validation: must be array with string name/SNMP_ID/img/description; img valid filename.
$loadEntry = static function ($entry) use ($validFilename): ?array {
    if (!is_array($entry)) return null;
    $n = $entry['name'] ?? null; $s = $entry['SNMP_ID'] ?? null;
    $i = $entry['img'] ?? null;  $d = $entry['description'] ?? null;
    if (!is_string($n) || !is_string($s) || !is_string($i) || !is_string($d)) return null;
    if (!$validFilename($i)) return null;
    return ['name' => $n, 'SNMP_ID' => $s, 'img' => $i, 'description' => $d];
};

$ctrlKeyCases = [
    ['750-880',          true],
    ['750-511/000-002',  true],
    ['750-5xx',          true],
    ['750-0880',         true],
    ['abc-123',          false], // must start with digit
    ['../etc/passwd',    false], // path traversal
    ['/etc/passwd',      false], // absolute path
    ['750 880',          false], // space not allowed
    ['',                 false], // empty
];
// v2.0: module keys are article names (same regex as controller keys), not slot integers.
$modKeyCases = [
    ['750-400',         true],  // standard article name
    ['750-511/000-002', true],  // article with slash
    ['750-5xx',         true],  // family identifier
    ['1405',            true],  // single-segment numeric (starts with digit)
    ['abc-123',         false], // must start with digit
    ['/750-400',        false], // starts with slash
    ['',                false], // empty
    ['750 880',         false], // space not allowed
];
$filenameCases = [
    ['wago_0750-0880.svg',   true],
    ['my_custom-module.svg', true],
    ['a.svg',                true],
    ['../etc/passwd.svg',    false], // path separator
    ['/abs/path.svg',        false], // absolute path
    ['no-extension',         false], // no .svg
    ['double..dot.svg',      false], // double dot
    ['file.SVG',             false], // uppercase extension
    ['.hidden.svg',          false], // starts with dot
    ['',                     false], // empty
];
$entryCases = [
    // Valid entry.
    [['name'=>'750-880','SNMP_ID'=>'750-880','img'=>'wago_0750-0880.svg','description'=>'Controller'], true],
    // Valid entry with empty name/description (strings are the requirement, not non-empty).
    [['name'=>'','SNMP_ID'=>'','img'=>'valid-file.svg','description'=>''], true],
    // Missing field.
    [['name'=>'750-880','SNMP_ID'=>'750-880','img'=>'file.svg'], false],
    // Invalid img filename.
    [['name'=>'x','SNMP_ID'=>'x','img'=>'../escape.svg','description'=>'x'], false],
    // Non-string field.
    [['name'=>750,'SNMP_ID'=>'750-880','img'=>'file.svg','description'=>'x'], false],
    // Not an array.
    ['just-a-string', false],
];
$ok = true;
foreach ($ctrlKeyCases as [$input, $expected]) {
    $got = $validCtrlKey($input);
    if ((bool)$got !== $expected) {
        echo "FAIL ctrlKey(" . json_encode($input) . ") = " . ($got?'true':'false') . ", want " . ($expected?'true':'false') . "\n";
        $ok = false;
    }
}
foreach ($modKeyCases as [$input, $expected]) {
    $got = $validModKey($input);
    if ((bool)$got !== $expected) {
        echo "FAIL modKey(" . json_encode($input) . ") = " . ($got?'true':'false') . ", want " . ($expected?'true':'false') . "\n";
        $ok = false;
    }
}
foreach ($filenameCases as [$input, $expected]) {
    $got = $validFilename($input);
    if ((bool)$got !== $expected) {
        echo "FAIL filename(" . json_encode($input) . ") = " . ($got?'true':'false') . ", want " . ($expected?'true':'false') . "\n";
        $ok = false;
    }
}
foreach ($entryCases as [$input, $expected]) {
    $got = $loadEntry($input);
    $isValid = ($got !== null);
    if ($isValid !== $expected) {
        echo "FAIL entry(" . json_encode($input) . ") = " . ($isValid?'valid':'null') . ", want " . ($expected?'valid':'null') . "\n";
        $ok = false;
    }
}
echo $ok ? "OK\n" : "FAILURES\n";`

	if got := runPhp(t, php, phpLogic); got != "OK" {
		t.Fatalf("schema validation failures:\n%s", got)
	}
}

// TestCustomMapJSONParsing verifies that absent/malformed JSON causes silent fallback
// (returns empty maps) and that the v2.0 catalog-entry schema is correctly loaded.
// v2.0: module keys are article names; entries include type and process_image.
func TestCustomMapJSONParsing(t *testing.T) {
	php := phpSkip(t)

	phpLogic := `
// Mirrors $loadConfigMap from widget.view.php (without file I/O or img_path resolution).
function loadFromJSON(?string $raw): array {
    $ctrl = []; $mod = [];
    if ($raw === null) return [$ctrl, $mod];
    $parsed = json_decode($raw, true);
    if (!is_array($parsed)) return [$ctrl, $mod];
    // v2.0: module keys are article names, same regex as controller keys.
    $validCatalogKey = fn(string $k) => (bool) preg_match('/^[0-9][0-9a-zA-Z\/\-]*$/', $k);
    $validFilename   = fn(string $f) => (bool) preg_match('/^[a-zA-Z0-9][a-zA-Z0-9_\-]*\.svg$/', $f);
    $loadEntry = function($entry) use ($validFilename) {
        if (!is_array($entry)) return null;
        $n = $entry['name'] ?? null; $s = $entry['SNMP_ID'] ?? null;
        $i = $entry['img'] ?? null;  $d = $entry['description'] ?? null;
        if (!is_string($n) || !is_string($s) || !is_string($i) || !is_string($d)) return null;
        if (!$validFilename($i)) return null;
        return ['SNMP_ID'=>$s,'name'=>$n,'img'=>$i,'description'=>$d];
    };
    if (isset($parsed['controllers']) && is_array($parsed['controllers'])) {
        foreach ($parsed['controllers'] as $k => $entry) {
            if (is_string($k) && $validCatalogKey($k)) {
                $e = $loadEntry($entry); if ($e !== null) $ctrl[$k] = $e;
            }
        }
    }
    if (isset($parsed['modules']) && is_array($parsed['modules'])) {
        foreach ($parsed['modules'] as $k => $entry) {
            $k = (string)$k;
            if ($validCatalogKey($k)) {
                $e = $loadEntry($entry);
                $pi = is_array($entry) ? ($entry['process_image'] ?? null) : null;
                $signatureValid = is_array($pi);
                foreach (['analog_in','analog_out','digital_in','digital_out'] as $field) {
                    $signatureValid = $signatureValid && isset($pi[$field]) && is_int($pi[$field]) && $pi[$field] >= 0;
                }
                if ($e !== null && $k === $e['name'] && isset($entry['type']) && is_int($entry['type']) && $entry['type'] >= 0 && $signatureValid) {
                    $mod[$k] = $e;
                }
            }
        }
    }
    return [$ctrl, $mod];
}
$ok = true;
// Absent file → empty maps.
[$c, $m] = loadFromJSON(null);
if ($c !== [] || $m !== []) { echo "FAIL: absent should give empty maps\n"; $ok = false; }
// Valid v2.0 JSON with article-keyed module and complete signature → populated.
$validJson = '{"controllers":{"750-880":{"SNMP_ID":"750-880","name":"750-880","img":"wago_0750-0880.svg","description":"ctrl"}},"modules":{"750-400":{"SNMP_ID":"750-4xx","name":"750-400","type":1,"img":"wago_0750-0400.svg","description":"mod","process_image":{"analog_in":0,"analog_out":0,"digital_in":2,"digital_out":0}}}}';
[$c, $m] = loadFromJSON($validJson);
if (!isset($c['750-880']) || !isset($m['750-400'])) { echo "FAIL: valid v2.0 JSON not parsed (c=" . count($c) . " m=" . count($m) . ")\n"; $ok = false; }
if (isset($c['750-880']) && $c['750-880']['SNMP_ID'] !== '750-880') { echo "FAIL: controller SNMP_ID wrong\n"; $ok = false; }
// Legacy slot-keyed configuration must not be interpreted as a v2.0 article catalog.
$slotKey = '{"modules":{"1":{"SNMP_ID":"750-4xx","name":"750-400","img":"wago_0750-0400.svg","description":"mod"}}}';
[, $m] = loadFromJSON($slotKey);
if ($m !== []) { echo "FAIL: legacy slot key must be rejected\n"; $ok = false; }
// Malformed JSON → empty maps (fail safe).
[$c, $m] = loadFromJSON('{not valid json}');
if ($c !== [] || $m !== []) { echo "FAIL: malformed should give empty maps\n"; $ok = false; }
// JSON without controllers/modules keys → empty maps.
[$c, $m] = loadFromJSON('{"other":"value"}');
if ($c !== [] || $m !== []) { echo "FAIL: missing sections should give empty maps\n"; $ok = false; }
// Wrong types for sections → empty maps.
[$c, $m] = loadFromJSON('{"controllers":"not-an-object","modules":42}');
if ($c !== [] || $m !== []) { echo "FAIL: wrong-type sections should give empty maps\n"; $ok = false; }
// Old v1.0 schema (string values) → entries skipped (strings are not valid entry objects).
$oldJson = '{"controllers":{"750-880":"wago_0750-0880.svg"},"modules":{"750-511":"wago_0750-0511.svg"}}';
[$c, $m] = loadFromJSON($oldJson);
if ($c !== [] || $m !== []) { echo "FAIL: old string-value schema should give empty maps\n"; $ok = false; }
echo $ok ? "OK\n" : "FAILURES\n";`

	if got := runPhp(t, php, phpLogic); got != "OK" {
		t.Fatalf("JSON parsing failures:\n%s", got)
	}
}

// TestCustomSvgPathSafety verifies the file-safety predicate rejects symlinks,
// non-existent files, files outside the images dir, and path-traversal filenames.
func TestCustomSvgPathSafety(t *testing.T) {
	php := phpSkip(t)

	// Create a temp dir with a real SVG, a symlink, and a file outside the images dir.
	tmpDir := t.TempDir()
	imagesDir := filepath.Join(tmpDir, "images")
	if err := os.MkdirAll(imagesDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write a real SVG file.
	realSvg := filepath.Join(imagesDir, "real.svg")
	if err := os.WriteFile(realSvg, []byte(`<svg viewBox="0 0 10 10"></svg>`), 0644); err != nil {
		t.Fatal(err)
	}
	// Write a file outside the images dir.
	outsideFile := filepath.Join(tmpDir, "outside.svg")
	if err := os.WriteFile(outsideFile, []byte(`<svg viewBox="0 0 10 10"></svg>`), 0644); err != nil {
		t.Fatal(err)
	}
	// Create a symlink inside images dir pointing outside.
	symlinkPath := filepath.Join(imagesDir, "link.svg")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Fatal(err)
	}

	phpLogic := `
$imagesDir = $argv[1];
$imagesReal = realpath($imagesDir);

$validFile = static function(string $filename) use ($imagesDir, $imagesReal): bool {
    if ($imagesReal === false) return false;
    $path = $imagesDir . '/' . $filename;
    if (is_link($path)) return false;
    if (!is_file($path) || !is_readable($path)) return false;
    $real = realpath($path);
    return $real !== false && str_starts_with($real, $imagesReal . '/');
};

$ok = true;
// Real SVG file → accepted.
if (!$validFile('real.svg')) { echo "FAIL: real.svg should be valid\n"; $ok = false; }
// Symlink → rejected.
if ($validFile('link.svg'))  { echo "FAIL: symlink should be rejected\n"; $ok = false; }
// Non-existent → rejected.
if ($validFile('no-such-file.svg')) { echo "FAIL: missing file should be rejected\n"; $ok = false; }
// Traversal via filename (already blocked by filename regex, but test defence-in-depth).
if ($validFile('../outside.svg')) { echo "FAIL: path traversal should be rejected\n"; $ok = false; }
echo $ok ? "OK\n" : "FAILURES\n";`

	out, err := exec.Command(php, "-r", phpLogic, "--", imagesDir).CombinedOutput()
	if err != nil {
		t.Fatalf("php: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "OK" {
		t.Fatalf("path safety failures:\n%s", got)
	}
}

// TestFallbackHintContent verifies that the hint PHP closures produce accurate v2.0
// guidance: catalog-based (not slot-specific) module hint, controller hint, images path,
// json filename, and HTML escaping.
func TestFallbackHintContent(t *testing.T) {
	php := phpSkip(t)

	phpLogic := `
$e = static fn($v) => htmlspecialchars((string) $v, ENT_QUOTES | ENT_SUBSTITUTE, 'UTF-8');
// v2.0: hint references the catalog (not a specific slot).
$moduleCatalogHint = static function(string $liveSnmpId) use ($e): string {
    if ($liveSnmpId === '') return '';
    return '<br>To add SVG: copy .svg to /var/lib/zabbix/wago_kbus/images/'
        . ' and add a modules entry in custom_svg_map.json for SNMP_ID '
        . $e('"' . $liveSnmpId . '"') . '.';
};
$controllerHint = static function(string $liveSnmpId) use ($e): string {
    if ($liveSnmpId === '') return '';
    return '<br>To customise: copy .svg to /var/lib/zabbix/wago_kbus/images/'
        . ' and add/update a controllers entry with SNMP_ID ' . $e('"' . $liveSnmpId . '"')
        . ' in custom_svg_map.json.';
};
$ok = true;
// Empty SNMP_ID → no hint.
if ($moduleCatalogHint('') !== '') {
    echo "FAIL: moduleCatalogHint empty SNMP_ID should produce empty hint\n"; $ok = false;
}
if ($controllerHint('') !== '') {
    echo "FAIL: controllerHint empty SNMP_ID should produce empty hint\n"; $ok = false;
}
// moduleCatalogHint: must contain SNMP_ID, images path, json filename.
// Must NOT contain slot-specific keys like modules["5"].
$hint = $moduleCatalogHint('750-999');
if (strpos($hint, '750-999') === false) {
    echo "FAIL: moduleCatalogHint missing live SNMP_ID\n"; $ok = false;
}
if (strpos($hint, '/var/lib/zabbix/wago_kbus/images/') === false) {
    echo "FAIL: moduleCatalogHint missing images path\n"; $ok = false;
}
if (strpos($hint, 'custom_svg_map.json') === false) {
    echo "FAIL: moduleCatalogHint missing map filename\n"; $ok = false;
}
// v2.0: no slot number should appear in module hint.
if (preg_match('/modules\["?\d+"?\]/', $hint)) {
    echo "FAIL: moduleCatalogHint must not reference a slot-specific modules key\n"; $ok = false;
}
// controllerHint contains 'controllers', live SNMP_ID, images path, json filename.
$hint2 = $controllerHint('750-881');
if (strpos($hint2, 'controllers') === false) {
    echo "FAIL: controllerHint missing controllers reference\n"; $ok = false;
}
if (strpos($hint2, '750-881') === false) {
    echo "FAIL: controllerHint missing live SNMP_ID\n"; $ok = false;
}
if (strpos($hint2, '/var/lib/zabbix/wago_kbus/images/') === false) {
    echo "FAIL: controllerHint missing images path\n"; $ok = false;
}
if (strpos($hint2, 'custom_svg_map.json') === false) {
    echo "FAIL: controllerHint missing map filename\n"; $ok = false;
}
// HTML-special chars in SNMP_ID are escaped (XSS guard).
$hint3 = $moduleCatalogHint('<script>alert(1)</script>');
if (strpos($hint3, '<script>') !== false) {
    echo "FAIL: moduleCatalogHint did not escape HTML in SNMP_ID\n"; $ok = false;
}
$hint4 = $controllerHint('<script>xss</script>');
if (strpos($hint4, '<script>') !== false) {
    echo "FAIL: controllerHint did not escape HTML in SNMP_ID\n"; $ok = false;
}
echo $ok ? "OK\n" : "FAILURES\n";`

	if got := runPhp(t, php, phpLogic); got != "OK" {
		t.Fatalf("fallback hint failures:\n%s", got)
	}
}

// TestInstallerCustomDataPersistence verifies that install_widget.sh:
//
//	(a) creates custom data dirs and starter JSON on first run,
//	(b) never overwrites or deletes them on a second run.
//
// Uses WAGO_KBUS_DATA_DIR to redirect custom data to a temp directory so the
// test does not require root or write to /var/lib.
func TestInstallerCustomDataPersistence(t *testing.T) {
	tmpRoot := t.TempDir()
	modulesDir := filepath.Join(tmpRoot, "zabbix", "modules")
	dataDir := filepath.Join(tmpRoot, "wago_kbus_data")
	if err := os.MkdirAll(modulesDir, 0755); err != nil {
		t.Fatal(err)
	}

	script, err := filepath.Abs(filepath.Join("..", "scripts", "install_widget.sh"))
	if err != nil {
		t.Fatal(err)
	}

	run := func(label string) {
		t.Helper()
		cmd := exec.Command("bash", script, "--zabbix-modules-dir", modulesDir)
		cmd.Env = append(os.Environ(), "WAGO_KBUS_DATA_DIR="+dataDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", label, err, out)
		}
	}

	mapFile := filepath.Join(dataDir, "custom_svg_map.json")
	imagesDir := filepath.Join(dataDir, "images")
	markerFile := filepath.Join(imagesDir, "custom_marker.svg")

	// --- First install -------------------------------------------------------
	run("first install")

	if _, err := os.Stat(mapFile); err != nil {
		t.Fatalf("custom_svg_map.json not created on first install: %v", err)
	}
	if _, err := os.Stat(imagesDir); err != nil {
		t.Fatalf("images dir not created on first install: %v", err)
	}

	// Validate starter JSON has correct schema.
	raw, err := os.ReadFile(mapFile)
	if err != nil {
		t.Fatalf("cannot read custom_svg_map.json: %v", err)
	}
	var starter map[string]any
	if err := json.Unmarshal(raw, &starter); err != nil {
		t.Fatalf("starter custom_svg_map.json is not valid JSON: %v", err)
	}
	if _, ok := starter["controllers"]; !ok {
		t.Error("starter JSON missing 'controllers' key")
	}
	if _, ok := starter["modules"]; !ok {
		t.Error("starter JSON missing 'modules' key")
	}
	// v2.0 starter: 6-article catalog (not the old 19-slot installation inventory).
	if mods, ok := starter["modules"].(map[string]any); ok {
		if len(mods) != 6 {
			t.Errorf("starter JSON has %d module entries, want 6 (v2.0 article catalog)", len(mods))
		}
	} else {
		t.Error("starter JSON 'modules' is not an object")
	}
	// Shipped SVG images must be present in images dir after first install.
	shippedImages := []string{
		"wago_0750-0880.svg",
		"wago_0750-0511.svg",
		"wago_0750-0600.svg",
		"wago_0750-xxxx_controller.svg",
		"wago_0750-xxxx_modul.svg",
	}
	for _, img := range shippedImages {
		imgPath := filepath.Join(imagesDir, img)
		if _, err := os.Stat(imgPath); err != nil {
			t.Errorf("shipped image not copied on first install: %s", imgPath)
		}
	}

	// Modify both custom files to simulate admin-added content.
	customMap := `{"controllers":{"750-881":"custom_ctrl.svg"},"modules":{"750-999":"custom_mod.svg"}}`
	if err := os.WriteFile(mapFile, []byte(customMap), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerFile, []byte(`<svg viewBox="0 0 1 1"></svg>`), 0644); err != nil {
		t.Fatal(err)
	}

	// --- Second install (update) ---------------------------------------------
	run("second install")

	// Custom map must not have been overwritten.
	got, err := os.ReadFile(mapFile)
	if err != nil {
		t.Fatalf("cannot read custom_svg_map.json after second install: %v", err)
	}
	if string(got) != customMap {
		t.Errorf("custom_svg_map.json was overwritten:\n  got:  %q\n  want: %q", got, customMap)
	}

	// Custom SVG marker must still exist.
	if _, err := os.Stat(markerFile); err != nil {
		t.Errorf("custom SVG marker was deleted on second install: %v", err)
	}

	// Widget module must still be present and not nested.
	widgetDest := filepath.Join(modulesDir, "wago_kbus")
	if _, err := os.Stat(filepath.Join(widgetDest, "manifest.json")); err != nil {
		t.Errorf("manifest.json missing after second install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(widgetDest, "wago_kbus")); err == nil {
		t.Error("nested wago_kbus/wago_kbus directory detected after second install")
	}
}

func TestInstallerRejectsPersistentSymlinkDestinations(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "scripts", "install_widget.sh"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("data directory", func(t *testing.T) {
		root := t.TempDir()
		modulesDir := filepath.Join(root, "modules")
		outside := filepath.Join(root, "outside")
		dataDir := filepath.Join(root, "data")
		if err := os.MkdirAll(modulesDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(outside, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, dataDir); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", script, "--zabbix-modules-dir", modulesDir)
		cmd.Env = append(os.Environ(), "WAGO_KBUS_DATA_DIR="+dataDir)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("installer accepted symlinked data directory:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(outside, "custom_svg_map.json")); !os.IsNotExist(err) {
			t.Fatalf("installer wrote through data-directory symlink: %v", err)
		}
	})

	for _, target := range []string{"images", "custom_svg_map.json", "images/wago_0750-0880.svg"} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			modulesDir := filepath.Join(root, "modules")
			dataDir := filepath.Join(root, "data")
			outside := filepath.Join(root, "outside")
			if err := os.MkdirAll(modulesDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dataDir, "images"), 0755); err != nil {
				t.Fatal(err)
			}
			if target == "images" {
				if err := os.Remove(filepath.Join(dataDir, "images")); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(outside, 0755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(outside, []byte("sentinel"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dataDir, target)); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", script, "--zabbix-modules-dir", modulesDir)
			cmd.Env = append(os.Environ(), "WAGO_KBUS_DATA_DIR="+dataDir)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("installer accepted symlinked persistent target %s:\n%s", target, out)
			}
		})
	}
}

func TestInstallerRejectsOverlappingPersistentPath(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "scripts", "install_widget.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, relation := range []string{"equal", "nested", "ancestor"} {
		t.Run(relation, func(t *testing.T) {
			root := t.TempDir()
			modulesDir := filepath.Join(root, "modules")
			widgetDest := filepath.Join(modulesDir, "wago_kbus")
			dataDir := widgetDest
			switch relation {
			case "nested":
				dataDir = filepath.Join(widgetDest, "site-data")
			case "ancestor":
				dataDir = modulesDir
			}
			if err := os.MkdirAll(dataDir, 0755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(dataDir, "custom_svg_map.json")
			const marker = "persistent-sentinel"
			if err := os.WriteFile(sentinel, []byte(marker), 0644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", script, "--zabbix-modules-dir", modulesDir)
			cmd.Env = append(os.Environ(), "WAGO_KBUS_DATA_DIR="+dataDir)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("installer accepted %s persistent/widget overlap:\n%s", relation, out)
			}
			content, err := os.ReadFile(sentinel)
			if err != nil {
				t.Fatalf("sentinel deleted for %s overlap: %v", relation, err)
			}
			if string(content) != marker {
				t.Fatalf("sentinel changed for %s overlap: %q", relation, content)
			}
		})
	}
}
