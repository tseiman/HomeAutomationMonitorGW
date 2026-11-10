// File: zabbix/widget_test.go
// Purpose: Focused tests for the wago_kbus custom widget: manifest validity, PHP syntax,
// kbus_layout item JavaScript logic, and module-to-SVG matching rules.

package zabbix

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const widgetDir = "modules/wago_kbus"

func runKbusLayoutPreprocessing(t *testing.T, snapshot []byte) map[string]any {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	data, err := os.ReadFile("template_homeauthmonitorgw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document templateDocument
	if err := parseYAMLTemplate(data, &document); err != nil {
		t.Fatalf("parse template: %v", err)
	}
	var scriptBody string
	for _, item := range document.Export.Templates[0].Items {
		if item.Key != "automation.gateway.wago.kbus_layout" {
			continue
		}
		for _, step := range item.Preprocessing {
			if step.Type == "JAVASCRIPT" {
				scriptBody = step.Parameters[0]
			}
		}
	}
	if scriptBody == "" {
		t.Fatal("kbus_layout JAVASCRIPT preprocessing not found")
	}
	scriptBody = strings.ReplaceAll(scriptBody, "{$WAGO_SOURCE}", "wago-test")
	script := "const value = process.argv[1]; function transform() {\n" + scriptBody + "\n} process.stdout.write(String(transform()));"
	output, err := exec.Command(node, "-e", script, fullPollProjection(t, string(snapshot))).CombinedOutput()
	if err != nil {
		t.Fatalf("kbus_layout JS: %v: %s", err, output)
	}
	var layout map[string]any
	if err := json.Unmarshal(output, &layout); err != nil {
		t.Fatalf("kbus_layout output is not JSON: %v — %q", err, output)
	}
	return layout
}

// TestWidgetManifestIsValid checks that manifest.json parses as valid JSON and contains
// the required top-level fields for a Zabbix 7.4 custom widget.
func TestWidgetManifestIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(widgetDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("manifest.json is not valid JSON: %v", err)
	}
	required := []string{"manifest_version", "id", "type", "name", "namespace", "version", "widget", "actions", "assets"}
	for _, key := range required {
		if _, ok := manifest[key]; !ok {
			t.Errorf("manifest.json missing required key %q", key)
		}
	}
	if manifest["id"] != "wago_kbus" {
		t.Errorf("manifest id=%q, want wago_kbus", manifest["id"])
	}
	if manifest["type"] != "widget" {
		t.Errorf("manifest type=%q, want widget", manifest["type"])
	}
	widget, ok := manifest["widget"].(map[string]any)
	if !ok {
		t.Fatal("manifest widget section is not an object")
	}
	if widget["js_class"] != "CWidgetWagoKbus" {
		t.Errorf("manifest widget.js_class=%q, want CWidgetWagoKbus", widget["js_class"])
	}
}

// TestWidgetPhpSyntax runs php -l on every PHP file in the widget directory.
func TestWidgetPhpSyntax(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not installed — skipping PHP syntax check")
	}
	phpFiles, err := filepath.Glob(filepath.Join(widgetDir, "**/*.php"))
	if err != nil {
		t.Fatal(err)
	}
	// Glob doesn't recurse; walk manually.
	var found []string
	for _, pattern := range []string{
		filepath.Join(widgetDir, "*.php"),
		filepath.Join(widgetDir, "actions/*.php"),
		filepath.Join(widgetDir, "includes/*.php"),
		filepath.Join(widgetDir, "views/*.php"),
	} {
		matches, _ := filepath.Glob(pattern)
		found = append(found, matches...)
	}
	_ = phpFiles
	if len(found) == 0 {
		t.Fatal("no PHP files found in widget directory")
	}
	for _, path := range found {
		t.Run(filepath.Base(path), func(t *testing.T) {
			out, err := exec.Command(php, "-l", path).CombinedOutput()
			if err != nil {
				t.Fatalf("php -l %s: %v\n%s", path, err, out)
			}
		})
	}
}

// TestWidgetSvgAssetsExist verifies that the required SVG files are present in assets/img.
func TestWidgetSvgAssetsExist(t *testing.T) {
	required := []string{
		"wago_0750-0880.svg",
		"wago_0750-0511.svg",
		"wago_0750-0600.svg",
		"wago_0750-xxxx_controller.svg",
		"wago_0750-xxxx_modul.svg",
	}
	for _, name := range required {
		path := filepath.Join(widgetDir, "assets/img", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("required SVG asset missing: %s", path)
		}
	}
}

// TestWidgetSvgAssetsAreWellFormed checks basic SVG validity (XML header + svg element).
func TestWidgetSvgAssetsAreWellFormed(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(widgetDir, "assets/img/*.svg"))
	if len(files) == 0 {
		t.Fatal("no SVG files found in assets/img")
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("cannot read %s: %v", path, err)
			continue
		}
		content := string(data)
		if !strings.Contains(content, "<svg") {
			t.Errorf("%s does not contain an <svg element", filepath.Base(path))
		}
		if strings.Contains(content, "<script") {
			t.Errorf("%s contains a <script element (unexpected in static SVG assets)", filepath.Base(path))
		}
		// Check that the SVG declares viewBox (required for proper auto-scaling).
		if !strings.Contains(content, "viewBox") {
			t.Errorf("%s is missing viewBox attribute", filepath.Base(path))
		}
	}
}

// TestKbusLayoutJavaScript executes the kbus_layout item JavaScript preprocessing against
// sample data and verifies: controller, 4 modules in slot order, plus the v2.0 fields
// reported_count, count_mismatch, duplicate_slots, inventory_contiguous, error_group,
// error_code, and per-module process_image (all null when no PI metrics in snapshot).
func TestKbusLayoutJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed — skipping kbus_layout JS test")
	}
	data, err := os.ReadFile("template_homeauthmonitorgw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document templateDocument
	if err := parseYAMLTemplate(data, &document); err != nil {
		t.Fatalf("parse template: %v", err)
	}
	template := document.Export.Templates[0]

	var scriptBody string
	for _, item := range template.Items {
		if item.Key != "automation.gateway.wago.kbus_layout" {
			continue
		}
		for _, step := range item.Preprocessing {
			if step.Type == "JAVASCRIPT" {
				scriptBody = step.Parameters[0]
			}
		}
	}
	if scriptBody == "" {
		t.Fatal("kbus_layout JAVASCRIPT preprocessing not found in template")
	}
	scriptBody = strings.ReplaceAll(scriptBody, "{$WAGO_SOURCE}", "wago-test")

	// Metrics delivered in reversed index order to confirm ordering is by wioModuleNumber value.
	snapshot := `{"wago-test":{"driver":"snmp","available":true,"stale":false,"last_success":"2026-10-05T00:00:00Z","metrics":[` +
		`{"name":"wioArticleName","value":"750-880"},` +
		`{"name":"wioModulCount","value":4},` +
		`{"name":"wioModuleDigitalInLength[1]","value":null},` +
		`{"name":"wioModuleNumber[4]","value":4},{"name":"wioModuleName[4]","value":"750-652/000-000"},{"name":"wioModuleType[4]","value":163},` +
		`{"name":"wioModuleNumber[3]","value":3},{"name":"wioModuleName[3]","value":"750-5xx"},{"name":"wioModuleType[3]","value":20},` +
		`{"name":"wioModuleNumber[2]","value":2},{"name":"wioModuleName[2]","value":"750-511/000-002"},{"name":"wioModuleType[2]","value":10},` +
		`{"name":"wioModuleNumber[1]","value":1},{"name":"wioModuleName[1]","value":"750-4xx"},{"name":"wioModuleType[1]","value":1}` +
		`]}}`

	script := "const value = process.argv[1]; function transform() {\n" + scriptBody + "\n} process.stdout.write(String(transform()));"
	output, err := exec.Command(node, "-e", script, fullPollProjection(t, snapshot)).CombinedOutput()
	if err != nil {
		t.Fatalf("kbus_layout JS: %v: %s", err, output)
	}

	// v2.0 layout struct includes process_image per module and global inventory fields.
	var layout struct {
		Controller          string `json:"controller"`
		ReportedCount       *int   `json:"reported_count"`
		CountMismatch       bool   `json:"count_mismatch"`
		DuplicateSlots      bool   `json:"duplicate_slots"`
		InventoryContiguous bool   `json:"inventory_contiguous"`
		ErrorGroup          *int   `json:"error_group"`
		ErrorCode           *int   `json:"error_code"`
		Modules             []struct {
			Slot         int    `json:"slot"`
			Article      string `json:"article"`
			Type         *int   `json:"type"`
			ProcessImage struct {
				AnalogIn   interface{} `json:"analog_in"`
				AnalogOut  interface{} `json:"analog_out"`
				DigitalIn  interface{} `json:"digital_in"`
				DigitalOut interface{} `json:"digital_out"`
			} `json:"process_image"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(output, &layout); err != nil {
		t.Fatalf("kbus_layout output not valid JSON: %v — output=%q", err, output)
	}
	if layout.Controller != "750-880" {
		t.Errorf("controller=%q, want 750-880", layout.Controller)
	}
	// Verify new v2.0 inventory fields.
	if layout.ReportedCount == nil || *layout.ReportedCount != 4 {
		t.Errorf("reported_count=%v, want 4", layout.ReportedCount)
	}
	if layout.CountMismatch {
		t.Error("count_mismatch=true, want false (4 reported, 4 actual)")
	}
	if layout.DuplicateSlots {
		t.Error("duplicate_slots=true, want false")
	}
	if !layout.InventoryContiguous {
		t.Error("inventory_contiguous=false, want true (slots 1..4)")
	}
	if len(layout.Modules) != 4 {
		t.Fatalf("module count=%d, want 4; modules=%+v", len(layout.Modules), layout.Modules)
	}
	// Verify slot order despite reversed metric delivery (scenario 2).
	cases := []struct {
		slot    int
		article string
	}{{1, "750-4xx"}, {2, "750-511/000-002"}, {3, "750-5xx"}, {4, "750-652/000-000"}}
	for i, want := range cases {
		got := layout.Modules[i]
		if got.Slot != want.slot || got.Article != want.article {
			t.Errorf("module[%d]: slot=%d article=%q, want slot=%d article=%q", i, got.Slot, got.Article, want.slot, want.article)
		}
		// process_image must be present; without PI metrics in snapshot all values must be null.
		if got.ProcessImage.AnalogIn != nil || got.ProcessImage.AnalogOut != nil ||
			got.ProcessImage.DigitalIn != nil || got.ProcessImage.DigitalOut != nil {
			t.Errorf("module[%d]: expected all process_image null (no PI metrics in snapshot)", i)
		}
	}
}

// TestCurrentWagoSnapshotInventory runs the production preprocessor against the sanitized
// inventory and diagnostic subset of the user-supplied gateway snapshot. The snapshot has
// 19 modules but no process-image length metrics, so those values must remain unknown.
func TestCurrentWagoSnapshotInventory(t *testing.T) {
	snapshot, err := os.ReadFile("testdata/current_wago_kbus_snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	layout := runKbusLayoutPreprocessing(t, snapshot)
	if layout["controller"] != "750-880" {
		t.Errorf("controller=%v, want 750-880", layout["controller"])
	}
	if layout["reported_count"] != float64(19) {
		t.Errorf("reported_count=%v, want 19", layout["reported_count"])
	}
	if valid, _ := layout["inventory_valid"].(bool); !valid {
		t.Errorf("inventory_valid=%v, want true", layout["inventory_valid"])
	}
	if layout["error_group"] != float64(0) || layout["error_code"] != float64(0) {
		t.Errorf("diagnostics group/code=%v/%v, want 0/0", layout["error_group"], layout["error_code"])
	}
	modules, ok := layout["modules"].([]any)
	if !ok || len(modules) != 19 {
		t.Fatalf("modules=%T len=%d, want 19", layout["modules"], len(modules))
	}
	for index, raw := range modules {
		module := raw.(map[string]any)
		if module["slot"] != float64(index+1) {
			t.Errorf("module[%d].slot=%v, want %d", index, module["slot"], index+1)
		}
		processImage := module["process_image"].(map[string]any)
		for _, field := range []string{"analog_in", "analog_out", "digital_in", "digital_out"} {
			if processImage[field] != nil {
				t.Errorf("module[%d].process_image.%s=%v, want null", index, field, processImage[field])
			}
		}
	}
}

func TestKbusLayoutRejectsDuplicateFieldsAndBlankNumbers(t *testing.T) {
	snapshot := []byte(`{"wago-test":{"metrics":[` +
		`{"name":"wioArticleName","value":"750-880"},` +
		`{"name":"wioModulCount","value":1},` +
		`{"name":"wioModuleNumber[1]","value":1},` +
		`{"name":"wioModuleNumber[1]","value":1},` +
		`{"name":"wioModuleName[1]","value":"750-4xx"},` +
		`{"name":"wioModuleType[1]","value":1},` +
		`{"name":"wioModuleDigitalInLength[1]","value":""}` +
		`]}}`)
	layout := runKbusLayoutPreprocessing(t, snapshot)
	if duplicate, _ := layout["duplicate_module_fields"].(bool); !duplicate {
		t.Errorf("duplicate_module_fields=%v, want true", layout["duplicate_module_fields"])
	}
	if valid, _ := layout["inventory_valid"].(bool); valid {
		t.Error("inventory_valid=true with duplicate module field")
	}
	modules := layout["modules"].([]any)
	processImage := modules[0].(map[string]any)["process_image"].(map[string]any)
	if processImage["digital_in"] != nil {
		t.Errorf("blank digital input length became %v, want null", processImage["digital_in"])
	}
}

// TestKbusLayout19SlotsWithProcessImage exercises the dynamic 19-slot path (scenario 1)
// and verifies that process_image metrics are correctly collected per slot (scenarios 3-8
// precondition: PI data is included in the output when metrics are present).
func TestKbusLayout19SlotsWithProcessImage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	data, err := os.ReadFile("template_homeauthmonitorgw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document templateDocument
	if err := parseYAMLTemplate(data, &document); err != nil {
		t.Fatalf("parse template: %v", err)
	}
	var scriptBody string
	for _, item := range document.Export.Templates[0].Items {
		if item.Key != "automation.gateway.wago.kbus_layout" {
			continue
		}
		for _, step := range item.Preprocessing {
			if step.Type == "JAVASCRIPT" {
				scriptBody = step.Parameters[0]
			}
		}
	}
	if scriptBody == "" {
		t.Fatal("kbus_layout JS not found")
	}
	scriptBody = strings.ReplaceAll(scriptBody, "{$WAGO_SOURCE}", "wago-test")

	// Build a 19-slot snapshot with metrics in reverse order to confirm slot ordering.
	// Slots 1 and 5 have explicit process_image metrics.
	var parts []string
	parts = append(parts,
		`{"name":"wioArticleName","value":"750-880"}`,
		`{"name":"wioModulCount","value":19}`,
		`{"name":"wioModuleDigitalInLength[1]","value":2}`,
		`{"name":"wioModuleDigitalOutLength[1]","value":0}`,
		`{"name":"wioModuleAnalogInLength[1]","value":0}`,
		`{"name":"wioModuleAnalogOutLength[1]","value":0}`,
		`{"name":"wioModuleDigitalInLength[5]","value":16}`,
	)
	// Add 19 modules in reverse metric order to test sorting.
	for i := 19; i >= 1; i-- {
		numStr := strconv.Itoa(i)
		name := "750-4xx"
		tp := "1"
		if i%2 == 0 {
			name = "750-5xx"
			tp = "2"
		}
		parts = append(parts,
			`{"name":"wioModuleNumber[`+numStr+`]","value":`+numStr+`}`,
			`{"name":"wioModuleName[`+numStr+`]","value":"`+name+`"}`,
			`{"name":"wioModuleType[`+numStr+`]","value":`+tp+`}`,
		)
	}
	snapshot := `{"wago-test":{"driver":"snmp","available":true,"stale":false,"last_success":"2026-10-05T00:00:00Z","metrics":[` +
		strings.Join(parts, ",") + `]}}`

	script := "const value = process.argv[1]; function transform() {\n" + scriptBody + "\n} process.stdout.write(String(transform()));"
	output, err := exec.Command(node, "-e", script, fullPollProjection(t, snapshot)).CombinedOutput()
	if err != nil {
		t.Fatalf("19-slot JS: %v: %s", err, output)
	}

	var layout struct {
		Controller          string `json:"controller"`
		ReportedCount       *int   `json:"reported_count"`
		CountMismatch       bool   `json:"count_mismatch"`
		InventoryContiguous bool   `json:"inventory_contiguous"`
		Modules             []struct {
			Slot         int `json:"slot"`
			ProcessImage struct {
				AnalogIn   interface{} `json:"analog_in"`
				AnalogOut  interface{} `json:"analog_out"`
				DigitalIn  interface{} `json:"digital_in"`
				DigitalOut interface{} `json:"digital_out"`
			} `json:"process_image"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(output, &layout); err != nil {
		t.Fatalf("19-slot output: %v — %q", err, output)
	}
	if layout.Controller != "750-880" {
		t.Errorf("controller=%q, want 750-880", layout.Controller)
	}
	if layout.ReportedCount == nil || *layout.ReportedCount != 19 {
		t.Errorf("reported_count=%v, want 19", layout.ReportedCount)
	}
	if layout.CountMismatch {
		t.Error("count_mismatch=true, want false")
	}
	if !layout.InventoryContiguous {
		t.Error("inventory_contiguous=false, want true for slots 1..19")
	}
	if len(layout.Modules) != 19 {
		t.Fatalf("module count=%d, want 19", len(layout.Modules))
	}
	// Modules must be in ascending slot order (scenario 2: ordering by wioModuleNumber).
	for i, m := range layout.Modules {
		if m.Slot != i+1 {
			t.Errorf("module[%d].slot=%d, want %d (wrong sort order)", i, m.Slot, i+1)
		}
	}
	// Slot 1 must have process_image populated (DI=2 was in snapshot).
	slot1 := layout.Modules[0]
	if slot1.ProcessImage.DigitalIn == nil {
		t.Error("slot 1 digital_in must be non-null (metric present in snapshot)")
	}
	// Slot 3 must have null process_image (no PI metrics for slot 3 in snapshot).
	slot3 := layout.Modules[2]
	if slot3.ProcessImage.DigitalIn != nil || slot3.ProcessImage.DigitalOut != nil {
		t.Error("slot 3 process_image must be all null (no PI metrics for slot 3)")
	}
}

// TestKbusLayoutInventoryValidation tests the count_mismatch, duplicate_slots, and
// inventory_contiguous fields (scenarios 12, 13, 14).
func TestKbusLayoutInventoryValidation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	data, err := os.ReadFile("template_homeauthmonitorgw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document templateDocument
	if err := parseYAMLTemplate(data, &document); err != nil {
		t.Fatalf("parse template: %v", err)
	}
	var scriptBody string
	for _, item := range document.Export.Templates[0].Items {
		if item.Key != "automation.gateway.wago.kbus_layout" {
			continue
		}
		for _, step := range item.Preprocessing {
			if step.Type == "JAVASCRIPT" {
				scriptBody = step.Parameters[0]
			}
		}
	}
	if scriptBody == "" {
		t.Fatal("kbus_layout JS not found")
	}
	scriptBody = strings.ReplaceAll(scriptBody, "{$WAGO_SOURCE}", "wago-test")

	run := func(snapshot string) map[string]any {
		t.Helper()
		script := "const value = process.argv[1]; function transform() {\n" + scriptBody + "\n} process.stdout.write(String(transform()));"
		output, err := exec.Command(node, "-e", script, fullPollProjection(t, snapshot)).CombinedOutput()
		if err != nil {
			t.Fatalf("JS: %v: %s", err, output)
		}
		var layout map[string]any
		if err := json.Unmarshal(output, &layout); err != nil {
			t.Fatalf("bad JSON: %v — %q", err, output)
		}
		return layout
	}

	// Scenario 14: wioModulCount=3 but only 2 module entries → count_mismatch.
	snap14 := `{"wago-test":{"driver":"snmp","available":true,"stale":false,"last_success":"2026-10-05T00:00:00Z","metrics":[` +
		`{"name":"wioModulCount","value":3},` +
		`{"name":"wioModuleNumber[1]","value":1},{"name":"wioModuleName[1]","value":"750-4xx"},{"name":"wioModuleType[1]","value":1},` +
		`{"name":"wioModuleNumber[2]","value":2},{"name":"wioModuleName[2]","value":"750-5xx"},{"name":"wioModuleType[2]","value":2}` +
		`]}}`
	lay14 := run(snap14)
	if cm, _ := lay14["count_mismatch"].(bool); !cm {
		t.Errorf("scenario 14: count_mismatch=false, want true (reported 3, actual 2)")
	}

	// Scenario 13: duplicate wioModuleNumber values → duplicate_slots=true.
	snap13 := `{"wago-test":{"driver":"snmp","available":true,"stale":false,"last_success":"2026-10-05T00:00:00Z","metrics":[` +
		`{"name":"wioModulCount","value":2},` +
		`{"name":"wioModuleNumber[1]","value":3},{"name":"wioModuleName[1]","value":"750-4xx"},{"name":"wioModuleType[1]","value":1},` +
		`{"name":"wioModuleNumber[2]","value":3},{"name":"wioModuleName[2]","value":"750-5xx"},{"name":"wioModuleType[2]","value":2}` +
		`]}}`
	lay13 := run(snap13)
	if ds, _ := lay13["duplicate_slots"].(bool); !ds {
		t.Errorf("scenario 13: duplicate_slots=false, want true (two modules both at slot 3)")
	}

	// Scenario 12: non-contiguous slots (1, 3) → inventory_contiguous=false.
	snap12 := `{"wago-test":{"driver":"snmp","available":true,"stale":false,"last_success":"2026-10-05T00:00:00Z","metrics":[` +
		`{"name":"wioModulCount","value":2},` +
		`{"name":"wioModuleNumber[1]","value":1},{"name":"wioModuleName[1]","value":"750-4xx"},{"name":"wioModuleType[1]","value":1},` +
		`{"name":"wioModuleNumber[3]","value":3},{"name":"wioModuleName[3]","value":"750-5xx"},{"name":"wioModuleType[3]","value":2}` +
		`]}}`
	lay12 := run(snap12)
	if ic, _ := lay12["inventory_contiguous"].(bool); ic {
		t.Errorf("scenario 12: inventory_contiguous=true, want false (slots 1,3 are not contiguous)")
	}
}

// TestKbusLayoutMissingWioArticleName verifies that a snapshot without wioArticleName
// produces controller: null (widget will use fallback SVG).
func TestKbusLayoutMissingWioArticleName(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	data, err := os.ReadFile("template_homeauthmonitorgw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document templateDocument
	if err := parseYAMLTemplate(data, &document); err != nil {
		t.Fatalf("parse template: %v", err)
	}
	var scriptBody string
	for _, item := range document.Export.Templates[0].Items {
		if item.Key != "automation.gateway.wago.kbus_layout" {
			continue
		}
		for _, step := range item.Preprocessing {
			if step.Type == "JAVASCRIPT" {
				scriptBody = step.Parameters[0]
			}
		}
	}
	if scriptBody == "" {
		t.Fatal("kbus_layout JS not found")
	}
	scriptBody = strings.ReplaceAll(scriptBody, "{$WAGO_SOURCE}", "wago-test")
	snapshot := `{"wago-test":{"driver":"snmp","available":true,"stale":false,"last_success":"2026-10-05T00:00:00Z","metrics":[{"name":"wioModuleNumber[1]","value":1},{"name":"wioModuleName[1]","value":"750-4xx"},{"name":"wioModuleType[1]","value":1}]}}`
	script := "const value = process.argv[1]; function transform() {\n" + scriptBody + "\n} process.stdout.write(String(transform()));"
	output, err := exec.Command(node, "-e", script, fullPollProjection(t, snapshot)).CombinedOutput()
	if err != nil {
		t.Fatalf("JS: %v: %s", err, output)
	}
	var layout map[string]any
	if err := json.Unmarshal(output, &layout); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if layout["controller"] != nil {
		t.Errorf("controller=%v, want null when wioArticleName absent", layout["controller"])
	}
}

// TestDefaultJsonSchema verifies that default_svg_map.json is a valid v2.0 module catalog:
// article-keyed, exactly 6 module entries, each with SNMP_ID/name/type/img/description
// and a process_image block with the four bit-length values.
// Keys must be article names (start with digit, not pure integers) rather than slot numbers.
func TestDefaultJsonSchema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(widgetDir, "default_svg_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Controllers map[string]json.RawMessage `json:"controllers"`
		Modules     map[string]json.RawMessage `json:"modules"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("default_svg_map.json is not valid JSON: %v", err)
	}
	if len(m.Controllers) == 0 {
		t.Error("default_svg_map.json controllers section is empty")
	}
	// v2.0 catalog: exactly 6 reusable article entries, not the old 19-slot inventory.
	if len(m.Modules) != 6 {
		t.Errorf("default_svg_map.json has %d module entries, want 6 (v2.0 article catalog)", len(m.Modules))
	}
	type processImage struct {
		AnalogIn   *int `json:"analog_in"`
		AnalogOut  *int `json:"analog_out"`
		DigitalIn  *int `json:"digital_in"`
		DigitalOut *int `json:"digital_out"`
	}
	type ctrlEntry struct {
		SNMPID      string `json:"SNMP_ID"`
		Name        string `json:"name"`
		Img         string `json:"img"`
		Description string `json:"description"`
	}
	type modEntry struct {
		SNMPID       string        `json:"SNMP_ID"`
		Name         string        `json:"name"`
		Type         *int          `json:"type"`
		Img          string        `json:"img"`
		Description  string        `json:"description"`
		ProcessImage *processImage `json:"process_image"`
	}
	for key, raw := range m.Controllers {
		var e ctrlEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Errorf("controllers[%q] not a valid entry object: %v", key, err)
			continue
		}
		if e.SNMPID == "" || e.Name == "" || e.Img == "" {
			t.Errorf("controllers[%q] has empty required field (SNMP_ID=%q name=%q img=%q)", key, e.SNMPID, e.Name, e.Img)
		}
	}
	// Expected catalog articles.
	expectedArticles := map[string]bool{
		"750-400": true, "750-501": true, "750-1405": true,
		"750-1504": true, "750-511": true, "750-652": true,
	}
	for key, raw := range m.Modules {
		// Keys must be article names, not slot integers.
		if !expectedArticles[key] {
			t.Errorf("modules[%q] unexpected key — catalog must use article names, not slot numbers", key)
		}
		var e modEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Errorf("modules[%q] not a valid entry object: %v", key, err)
			continue
		}
		if e.SNMPID == "" || e.Name == "" || e.Img == "" {
			t.Errorf("modules[%q] has empty required field (SNMP_ID=%q name=%q img=%q)", key, e.SNMPID, e.Name, e.Img)
		}
		if e.Type == nil {
			t.Errorf("modules[%q] missing required type field", key)
		}
		if e.ProcessImage == nil {
			t.Errorf("modules[%q] missing required process_image field", key)
		} else {
			if e.ProcessImage.AnalogIn == nil || e.ProcessImage.AnalogOut == nil ||
				e.ProcessImage.DigitalIn == nil || e.ProcessImage.DigitalOut == nil {
				t.Errorf("modules[%q] process_image has null fields (all four must be present)", key)
			}
		}
	}
}

// TestCatalogModuleIdentification verifies the v2.0 catalog-based identification algorithm
// using inline PHP that mirrors the new widget.view.php matching logic.
// Covers task scenarios 3–8: all six catalog articles must resolve uniquely when
// SNMP_ID + type + the relevant process_image fields are all available.
func TestCatalogModuleIdentification(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not installed — skipping catalog identification test")
	}

	phpLogic := `
// New v2.0 article catalog — mirrors default_svg_map.json.
$catalog = [
    '750-400'  => ['SNMP_ID'=>'750-4xx',         'type'=>1,   'process_image'=>['analog_in'=>0,'analog_out'=>0,'digital_in'=>2, 'digital_out'=>0]],
    '750-501'  => ['SNMP_ID'=>'750-5xx',         'type'=>2,   'process_image'=>['analog_in'=>0,'analog_out'=>0,'digital_in'=>0, 'digital_out'=>2]],
    '750-1405' => ['SNMP_ID'=>'750-4xx',         'type'=>1,   'process_image'=>['analog_in'=>0,'analog_out'=>0,'digital_in'=>16,'digital_out'=>0]],
    '750-1504' => ['SNMP_ID'=>'750-5xx',         'type'=>2,   'process_image'=>['analog_in'=>0,'analog_out'=>0,'digital_in'=>0, 'digital_out'=>16]],
    '750-511'  => ['SNMP_ID'=>'750-511/000-002', 'type'=>167, 'process_image'=>['analog_in'=>64,'analog_out'=>64,'digital_in'=>0,'digital_out'=>0]],
    '750-652'  => ['SNMP_ID'=>'750-652/000-000', 'type'=>163, 'process_image'=>['analog_in'=>192,'analog_out'=>192,'digital_in'=>0,'digital_out'=>0]],
];

// Identification algorithm: SNMP_ID → type → process_image (available fields only).
// Returns ['state'=>'identified'|'ambiguous'|'unknown', 'article'=>string|null].
function identify(string $snmpId, ?int $type, array $livePi, array $catalog): array {
    // Step 1: SNMP_ID filter.
    $cands = array_filter($catalog, fn($e) => $e['SNMP_ID'] === $snmpId);
    if (empty($cands)) return ['state'=>'unknown','article'=>null];
    // Step 2: type filter.
    if ($type !== null) {
        $cands = array_filter($cands, fn($e) => $e['type'] === $type);
        if (empty($cands)) return ['state'=>'unknown','article'=>null,'candidates'=>[]];
    }
    // Step 3: process_image filter (skip null live fields).
    $piFields = ['analog_in','analog_out','digital_in','digital_out'];
    $avail = array_filter($piFields, fn($f) => $livePi[$f] !== null);
    if (!empty($avail)) {
        $byPi = array_filter($cands, function($e) use ($livePi,$avail) {
            foreach ($avail as $f) {
                if ($e['process_image'][$f] !== $livePi[$f]) return false;
            }
            return true;
        });
        if (!empty($byPi)) { $cands = $byPi; }
        else { return ['state'=>'unknown','article'=>null]; }
    }
    if (count($cands) === 1) return ['state'=>'identified','article'=>array_key_first($cands)];
    return ['state'=>'ambiguous','article'=>null];
}

$ok = true;

// Scenario 3: 750-4xx + type 1 + DI 2 → 750-400
$r = identify('750-4xx', 1, ['analog_in'=>0,'analog_out'=>0,'digital_in'=>2,'digital_out'=>0], $catalog);
if ($r['state'] !== 'identified' || $r['article'] !== '750-400') { echo "FAIL s3: " . json_encode($r) . "\n"; $ok=false; }

// Scenario 4: 750-4xx + type 1 + DI 16 → 750-1405
$r = identify('750-4xx', 1, ['analog_in'=>0,'analog_out'=>0,'digital_in'=>16,'digital_out'=>0], $catalog);
if ($r['state'] !== 'identified' || $r['article'] !== '750-1405') { echo "FAIL s4: " . json_encode($r) . "\n"; $ok=false; }

// Scenario 5: 750-5xx + type 2 + DO 2 → 750-501
$r = identify('750-5xx', 2, ['analog_in'=>0,'analog_out'=>0,'digital_in'=>0,'digital_out'=>2], $catalog);
if ($r['state'] !== 'identified' || $r['article'] !== '750-501') { echo "FAIL s5: " . json_encode($r) . "\n"; $ok=false; }

// Scenario 6: 750-5xx + type 2 + DO 16 → 750-1504
$r = identify('750-5xx', 2, ['analog_in'=>0,'analog_out'=>0,'digital_in'=>0,'digital_out'=>16], $catalog);
if ($r['state'] !== 'identified' || $r['article'] !== '750-1504') { echo "FAIL s6: " . json_encode($r) . "\n"; $ok=false; }

// Scenario 7: 750-511/000-002 + type 167 + AI 64 + AO 64 → 750-511
$r = identify('750-511/000-002', 167, ['analog_in'=>64,'analog_out'=>64,'digital_in'=>0,'digital_out'=>0], $catalog);
if ($r['state'] !== 'identified' || $r['article'] !== '750-511') { echo "FAIL s7: " . json_encode($r) . "\n"; $ok=false; }

// Scenario 8: 750-652/000-000 + type 163 + AI 192 + AO 192 → 750-652
$r = identify('750-652/000-000', 163, ['analog_in'=>192,'analog_out'=>192,'digital_in'=>0,'digital_out'=>0], $catalog);
if ($r['state'] !== 'identified' || $r['article'] !== '750-652') { echo "FAIL s8: " . json_encode($r) . "\n"; $ok=false; }

echo $ok ? "OK\n" : "FAILURES\n";`

	out, err := exec.Command(php, "-r", phpLogic).CombinedOutput()
	if err != nil {
		t.Fatalf("php: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("catalog identification failures:\n%s", out)
	}
}

// TestCatalogAmbiguousUnknownStates verifies identification states when data is incomplete
// or the signature is genuinely absent from the catalog.
// Covers task scenarios 9 (ambiguous without PI), 10 (unknown signature),
// 11 (duplicate catalog entries → ambiguous), 15 (null PI must not be treated as zero).
func TestCatalogAmbiguousUnknownStates(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not installed")
	}

	phpLogic := `
$catalog = [
    '750-400'  => ['SNMP_ID'=>'750-4xx','type'=>1,'process_image'=>['analog_in'=>0,'analog_out'=>0,'digital_in'=>2,'digital_out'=>0]],
    '750-1405' => ['SNMP_ID'=>'750-4xx','type'=>1,'process_image'=>['analog_in'=>0,'analog_out'=>0,'digital_in'=>16,'digital_out'=>0]],
];

function identify(string $snmpId, ?int $type, array $livePi, array $catalog): array {
    $cands = array_filter($catalog, fn($e) => $e['SNMP_ID'] === $snmpId);
    if (empty($cands)) return ['state'=>'unknown','article'=>null,'candidates'=>[]];
    if ($type !== null) {
        $cands = array_filter($cands, fn($e) => $e['type'] === $type);
        if (empty($cands)) return ['state'=>'unknown','article'=>null,'candidates'=>[]];
    }
    $piFields = ['analog_in','analog_out','digital_in','digital_out'];
    $avail = array_filter($piFields, fn($f) => $livePi[$f] !== null);
    if (!empty($avail)) {
        $byPi = array_filter($cands, function($e) use ($livePi,$avail) {
            foreach ($avail as $f) {
                if ($e['process_image'][$f] !== $livePi[$f]) return false;
            }
            return true;
        });
        if (!empty($byPi)) { $cands = $byPi; }
        else { return ['state'=>'unknown','article'=>null,'candidates'=>[]]; }
    }
    if (count($cands) === 1) return ['state'=>'identified','article'=>array_key_first($cands),'candidates'=>array_keys($cands)];
    return ['state'=>'ambiguous','article'=>null,'candidates'=>array_keys($cands)];
}

$ok = true;

// Scenario 9: 750-4xx + type 1 + all PI null → ambiguous (never guessed).
$r = identify('750-4xx', 1, ['analog_in'=>null,'analog_out'=>null,'digital_in'=>null,'digital_out'=>null], $catalog);
if ($r['state'] !== 'ambiguous') { echo "FAIL s9 (ambiguous without PI): " . $r['state'] . "\n"; $ok=false; }
if (count($r['candidates']) !== 2) { echo "FAIL s9 candidate count: " . count($r['candidates']) . "\n"; $ok=false; }

// Scenario 10: 750-4xx + type 1 + DI 8 (no catalog match) → unknown.
$r = identify('750-4xx', 1, ['analog_in'=>0,'analog_out'=>0,'digital_in'=>8,'digital_out'=>0], $catalog);
if ($r['state'] !== 'unknown') { echo "FAIL s10 (unknown signature): " . $r['state'] . "\n"; $ok=false; }

// Scenario 11: two catalog entries with identical signature → ambiguous.
$dupCatalog = [
    'A' => ['SNMP_ID'=>'750-4xx','type'=>1,'process_image'=>['analog_in'=>0,'analog_out'=>0,'digital_in'=>2,'digital_out'=>0]],
    'B' => ['SNMP_ID'=>'750-4xx','type'=>1,'process_image'=>['analog_in'=>0,'analog_out'=>0,'digital_in'=>2,'digital_out'=>0]],
];
$r = identify('750-4xx', 1, ['analog_in'=>0,'analog_out'=>0,'digital_in'=>2,'digital_out'=>0], $dupCatalog);
if ($r['state'] !== 'ambiguous') { echo "FAIL s11 (duplicate entries): " . $r['state'] . "\n"; $ok=false; }

// Scenario 15: null PI must not be treated as zero.
// If digital_in=null were treated as 0, it would not match DI=2 or DI=16 → unknown.
// Correct: null is absent → skip PI filter → ambiguous.
$r = identify('750-4xx', 1, ['analog_in'=>null,'analog_out'=>null,'digital_in'=>null,'digital_out'=>null], $catalog);
if ($r['state'] !== 'ambiguous') { echo "FAIL s15 (null≠0): expected ambiguous, got " . $r['state'] . "\n"; $ok=false; }

// Unknown SNMP_ID → unknown regardless of type/PI.
$r = identify('750-999', 1, ['analog_in'=>0,'analog_out'=>0,'digital_in'=>0,'digital_out'=>0], $catalog);
if ($r['state'] !== 'unknown') { echo "FAIL (unknown SNMP_ID): " . $r['state'] . "\n"; $ok=false; }

echo $ok ? "OK\n" : "FAILURES\n";`

	out, err := exec.Command(php, "-r", phpLogic).CombinedOutput()
	if err != nil {
		t.Fatalf("php: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("ambiguous/unknown state failures:\n%s", out)
	}
}

// TestGlobalDiagnosticNoSlotFault verifies that global K-bus error diagnostics are
// carried as global fields in the layout JSON and that no individual module slot is
// marked red based on wioErrorArgument (scenario 17).
func TestGlobalDiagnosticNoSlotFault(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	data, err := os.ReadFile("template_homeauthmonitorgw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document templateDocument
	if err := parseYAMLTemplate(data, &document); err != nil {
		t.Fatalf("parse template: %v", err)
	}
	var scriptBody string
	for _, item := range document.Export.Templates[0].Items {
		if item.Key != "automation.gateway.wago.kbus_layout" {
			continue
		}
		for _, step := range item.Preprocessing {
			if step.Type == "JAVASCRIPT" {
				scriptBody = step.Parameters[0]
			}
		}
	}
	if scriptBody == "" {
		t.Fatal("kbus_layout JS not found")
	}
	scriptBody = strings.ReplaceAll(scriptBody, "{$WAGO_SOURCE}", "wago-test")

	// Snapshot with wioErrorGroup=1, wioErrorCode=5, wioErrorArgument=6 (should not fault slot 6).
	snapshot := `{"wago-test":{"driver":"snmp","available":true,"stale":false,"last_success":"2026-10-05T00:00:00Z","metrics":[` +
		`{"name":"wioArticleName","value":"750-880"},` +
		`{"name":"wioModulCount","value":2},` +
		`{"name":"wioErrorGroup","value":1},` +
		`{"name":"wioErrorCode","value":5},` +
		`{"name":"wioErrorArgument","value":6},` +
		`{"name":"wioModuleNumber[1]","value":1},{"name":"wioModuleName[1]","value":"750-4xx"},{"name":"wioModuleType[1]","value":1},` +
		`{"name":"wioModuleNumber[2]","value":2},{"name":"wioModuleName[2]","value":"750-5xx"},{"name":"wioModuleType[2]","value":2}` +
		`]}}`

	script := "const value = process.argv[1]; function transform() {\n" + scriptBody + "\n} process.stdout.write(String(transform()));"
	output, err := exec.Command(node, "-e", script, fullPollProjection(t, snapshot)).CombinedOutput()
	if err != nil {
		t.Fatalf("kbus_layout JS: %v: %s", err, output)
	}

	var layout map[string]any
	if err := json.Unmarshal(output, &layout); err != nil {
		t.Fatalf("bad JSON: %v — %q", err, output)
	}
	// Global diagnostics must be carried in top-level fields, not per-module.
	if layout["error_group"] == nil {
		t.Error("layout missing error_group field")
	}
	if layout["error_code"] == nil {
		t.Error("layout missing error_code field")
	}
	// No per-module fault field must exist (individual slot red from wioErrorArgument is unverified).
	modules, _ := layout["modules"].([]any)
	for i, m := range modules {
		mod, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if _, hasFault := mod["fault"]; hasFault {
			t.Errorf("module[%d] has unexpected per-slot fault field (wioErrorArgument must not cause per-slot red)", i)
		}
	}
}

// TestCatalogExtensionNoSlotConfig verifies that adding a catalog entry requires no
// slot-specific JSON configuration (scenario 18): the identification algorithm locates
// the article by SNMP_ID + type + process_image without any positional mapping.
func TestCatalogExtensionNoSlotConfig(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not installed")
	}

	phpLogic := `
// Start with baseline catalog.
$catalog = [
    '750-400' => ['SNMP_ID'=>'750-4xx','type'=>1,'process_image'=>['analog_in'=>0,'analog_out'=>0,'digital_in'=>2,'digital_out'=>0]],
];

function identify(string $snmpId, ?int $type, array $livePi, array $catalog): array {
    $cands = array_filter($catalog, fn($e) => $e['SNMP_ID'] === $snmpId);
    if (empty($cands)) return ['state'=>'unknown'];
    if ($type !== null) {
        $cands = array_filter($cands, fn($e) => $e['type'] === $type);
        if (empty($cands)) return ['state'=>'unknown','article'=>null,'candidates'=>[]];
    }
    $piFields = ['analog_in','analog_out','digital_in','digital_out'];
    $avail = array_filter($piFields, fn($f) => $livePi[$f] !== null);
    if (!empty($avail)) {
        $byPi = array_filter($cands, function($e) use ($livePi,$avail) {
            foreach ($avail as $f) { if ($e['process_image'][$f] !== $livePi[$f]) return false; }
            return true;
        });
        if (!empty($byPi)) $cands = $byPi;
        else return ['state'=>'unknown'];
    }
    if (count($cands) === 1) return ['state'=>'identified','article'=>array_key_first($cands)];
    return ['state'=>'ambiguous'];
}

$ok = true;

// Before adding new entry: 750-4xx + DI=16 → unknown (no match yet).
$r = identify('750-4xx', 1, ['analog_in'=>0,'analog_out'=>0,'digital_in'=>16,'digital_out'=>0], $catalog);
if ($r['state'] !== 'unknown') { echo "FAIL pre-extension: " . $r['state'] . "\n"; $ok=false; }

// Add a new catalog entry — no slot number configured anywhere.
$catalog['750-1405'] = ['SNMP_ID'=>'750-4xx','type'=>1,'process_image'=>['analog_in'=>0,'analog_out'=>0,'digital_in'=>16,'digital_out'=>0]];

// After adding: 750-4xx + DI=16 → identified as 750-1405, no slot config required.
$r = identify('750-4xx', 1, ['analog_in'=>0,'analog_out'=>0,'digital_in'=>16,'digital_out'=>0], $catalog);
if ($r['state'] !== 'identified' || $r['article'] !== '750-1405') {
    echo "FAIL post-extension: " . json_encode($r) . "\n"; $ok=false;
}

// Original entry still works.
$r = identify('750-4xx', 1, ['analog_in'=>0,'analog_out'=>0,'digital_in'=>2,'digital_out'=>0], $catalog);
if ($r['state'] !== 'identified' || $r['article'] !== '750-400') {
    echo "FAIL post-extension original: " . json_encode($r) . "\n"; $ok=false;
}

echo $ok ? "OK\n" : "FAILURES\n";`

	out, err := exec.Command(php, "-r", phpLogic).CombinedOutput()
	if err != nil {
		t.Fatalf("php: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("catalog extension failures:\n%s", out)
	}
}

// TestJsonControllerResolution verifies that controller SNMP_ID matching is strict
// case-sensitive equality.  null article and unrecognised articles produce nil (fallback).
func TestJsonControllerResolution(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not installed")
	}
	phpLogic := `
// Mirror of the widget resolveController logic.
function resolveController(?string $liveArticle, array $configControllers): ?array {
    if ($liveArticle === null) return null;
	$matches = [];
    foreach ($configControllers as $entry) {
		if ($entry['SNMP_ID'] === $liveArticle) $matches[] = $entry;
    }
	return count($matches) === 1 ? reset($matches) : null;
}

$cfg = [
    '750-880' => ['SNMP_ID' => '750-880', 'name' => '750-880', 'description' => 'Controller'],
];
$ok = true;

// Exact match.
$r = resolveController('750-880', $cfg);
if ($r === null || $r['name'] !== '750-880') { echo "FAIL: exact match returned null or wrong name\n"; $ok = false; }

// Null article → null (fallback).
if (resolveController(null, $cfg) !== null) { echo "FAIL: null article should return null\n"; $ok = false; }

// Unrecognised article → null (fallback).
if (resolveController('750-881', $cfg) !== null) { echo "FAIL: 750-881 should return null\n"; $ok = false; }

// Case-sensitive: '750-880' does not match '750-8XX'.
if (resolveController('750-8XX', $cfg) !== null) { echo "FAIL: case-different should return null\n"; $ok = false; }

// Empty config → null.
if (resolveController('750-880', []) !== null) { echo "FAIL: empty config should return null\n"; $ok = false; }

// Duplicate SNMP_ID matches are ambiguous and must not identify a controller.
$duplicateCfg = $cfg;
$duplicateCfg['750-880-alt'] = ['SNMP_ID' => '750-880', 'name' => '750-880-alt', 'description' => 'duplicate'];
if (resolveController('750-880', $duplicateCfg) !== null) { echo "FAIL: duplicate matches should return null\n"; $ok = false; }

echo $ok ? "OK\n" : "FAILURES\n";`

	out, err := exec.Command(php, "-r", phpLogic).CombinedOutput()
	if err != nil {
		t.Fatalf("php: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("JSON controller resolution failures:\n%s", out)
	}
}

// TestEndModuleRenderingContract keeps the passive 750-600 outside runtime inventory and
// appends it after every discovered module only when the controller resolves uniquely.
func TestEndModuleRenderingContract(t *testing.T) {
	viewData, err := os.ReadFile(filepath.Join(widgetDir, "views/widget.view.php"))
	if err != nil {
		t.Fatal(err)
	}
	view := string(viewData)
	for _, want := range []string{
		"wago_0750-0600.svg",
		"750-600 (End module)",
		"750-600 — End module",
		"Passive K-bus terminator",
		"no SNMP identity or process data",
		"wago-kbus-item--end-module",
		`role="button"`,
		`aria-label="750-600 End module details"`,
		"$controllerIdentified = $ctrlEntry !== null",
		"if ($controllerIdentified)",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("widget.view.php missing end-module contract %q", want)
		}
	}
	moduleLoop := strings.Index(view, "foreach ($modules as $module)")
	endModule := strings.Index(view, "// Append the passive 750-600 after the runtime inventory.")
	if moduleLoop < 0 || endModule < 0 || endModule <= moduleLoop {
		t.Errorf("750-600 must be rendered after the runtime module loop: loop=%d end=%d", moduleLoop, endModule)
	}
	resolverStart := strings.Index(view, "$resolveController =")
	resolverEnd := strings.Index(view, "// Filter candidates")
	if resolverStart < 0 || resolverEnd <= resolverStart {
		t.Fatal("cannot isolate controller resolver")
	}
	resolver := view[resolverStart:resolverEnd]
	for _, want := range []string{"$matches = []", "count($matches) !== 1"} {
		if !strings.Contains(resolver, want) {
			t.Errorf("controller resolver missing unique-match guard %q", want)
		}
	}
	if strings.Contains(resolver, "return $entry;") {
		t.Error("controller resolver must not accept the first matching catalog entry")
	}

	catalogData, err := os.ReadFile(filepath.Join(widgetDir, "default_svg_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Modules map[string]json.RawMessage `json:"modules"`
	}
	if err := json.Unmarshal(catalogData, &catalog); err != nil {
		t.Fatal(err)
	}
	if _, found := catalog.Modules["750-600"]; found {
		t.Error("passive 750-600 must not be treated as a discovered/catalog-matched runtime module")
	}

	cssData, err := os.ReadFile(filepath.Join(widgetDir, "assets/css/widget.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssData)
	if !strings.Contains(css, "gap: 0") || !strings.Contains(css, ".wago-kbus-item + .wago-kbus-item") {
		t.Error("rail must keep controller, runtime modules, and end module directly adjacent")
	}
}

// TestNoHardcodedMapsInPhp checks that the redesigned widget.view.php no longer contains
// the removed hard-coded product maps or description arrays.
func TestNoHardcodedMapsInPhp(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(widgetDir, "views/widget.view.php"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	forbidden := []string{
		"$WAGO_DESCRIPTIONS", "$SVG_CONTROLLER_MAP", "$SVG_MODULE_MAP",
		"$configModules", "$allFallback", "$key = (string) $slot", "modules[\"' . $slot",
	}
	for _, s := range forbidden {
		if strings.Contains(content, s) {
			t.Errorf("widget.view.php still contains hard-coded or slot-keyed module logic: %q", s)
		}
	}
	for _, required := range []string{"$moduleCatalog", "$identifyModule", "process_image", "data-incomplete", "ambiguous", "unknown"} {
		if !strings.Contains(content, required) {
			t.Errorf("widget.view.php missing catalog identification contract %q", required)
		}
	}
}

// TestInstallerDefaultPath checks that install_widget.sh defaults to the correct Zabbix 7.4
// module path (/usr/share/zabbix/ui/modules, not the old /usr/share/zabbix/modules).
func TestInstallerDefaultPath(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "scripts", "install_widget.sh"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "/usr/share/zabbix/ui/modules") {
		t.Error("install_widget.sh does not reference /usr/share/zabbix/ui/modules as default path")
	}
	// The old path must not be the default.
	if strings.Contains(content, `ZABBIX_MODULES_DIR="/usr/share/zabbix/modules"`) {
		t.Error("install_widget.sh still sets old default path /usr/share/zabbix/modules")
	}
}

// TestWidgetTooltipPortalContracts guards the browser-side behavior that keeps
// tooltip content outside the widget's clipping containers and cleans it up on
// every Zabbix lifecycle transition.
func TestWidgetTooltipPortalContracts(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(widgetDir, "assets/js/class.widget.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)

	required := []string{
		"document.body.appendChild(tooltip)",
		"document.createElementNS(ns, 'svg')",
		"document.createElementNS(ns, 'line')",
		"document.createElementNS(ns, 'polygon')",
		"getBoundingClientRect()",
		"safeRight",
		"safeBottom",
		"setContents(data)",
		"this._kbusTeardown()",
		"this._kbusSetup()",
		"onResize()",
		"onDeactivate()",
		"onDestroy()",
		"document.addEventListener('scroll'",
		"window.addEventListener('resize'",
		"tooltip._wkbConnector.svg.remove()",
		"this._kbusPositionConnector(item, tooltip, panel)",
	}
	for _, want := range required {
		if !strings.Contains(src, want) {
			t.Errorf("class.widget.js missing tooltip portal/lifecycle contract %q", want)
		}
	}
}

func TestCodesysChannelTooltipContracts(t *testing.T) {
	viewData, err := os.ReadFile(filepath.Join(widgetDir, "views/widget.view.php"))
	if err != nil {
		t.Fatal(err)
	}
	view := string(viewData)
	for _, want := range []string{
		"/var/lib/zabbix/wago_kbus/plc_configuration.cfg",
		"is_link($path)",
		"2 * 1024 * 1024",
		`$parent['module_name'] === 'K-Bus'`,
		"substr(trim($value), 0, 512)",
		"$inventoryValid && $slot > 0",
		"wago-kbus-channel-list",
		"Configured I/O",
		"<strong>Channel ",
		"$e($configuredChannel['symbolic_name'])",
		"role=\"button\"",
		"aria-describedby=\"",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("widget.view.php missing CODESYS channel contract %q", want)
		}
	}

	jsData, err := os.ReadFile(filepath.Join(widgetDir, "assets/js/class.widget.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsData)
	for _, want := range []string{
		"Math.min(360, availableW)",
		"tooltip.style.maxHeight = availableH + 'px'",
		"tooltip.style.overflowY = 'auto'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("class.widget.js missing long-tooltip contract %q", want)
		}
	}

	installerData, err := os.ReadFile(filepath.Join("..", "scripts", "install_widget.sh"))
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, want := range []string{
		`PLC_CONFIG_FILE="${CUSTOM_DATA_DIR}/plc_configuration.cfg"`,
		`reject_symlink_components "${PLC_CONFIG_FILE}"`,
		`Exists (preserve): ${PLC_CONFIG_FILE}`,
	} {
		if !strings.Contains(installer, want) {
			t.Errorf("install_widget.sh missing CODESYS file contract %q", want)
		}
	}
}

// TestWidgetTooltipPositioningJavaScript executes the real positioning method
// with synthetic DOM rectangles. It covers right/left placement, center clamping,
// and a panel partially outside the viewport without requiring a browser download.
func TestWidgetTooltipPositioningJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed — skipping tooltip geometry test")
	}
	data, err := os.ReadFile(filepath.Join(widgetDir, "assets/js/class.widget.js"))
	if err != nil {
		t.Fatal(err)
	}

	script := `
globalThis.CWidget = class {};
globalThis.document = {documentElement: {clientWidth: 320, clientHeight: 240}};
eval(process.argv[1] + "\nglobalThis.TestWidget = CWidgetWagoKbus;");
const widget = new globalThis.TestWidget();

function check(name, panelRect, itemRect, tipRect, expectedSide) {
  const panel = {getBoundingClientRect: () => panelRect};
  const item = {getBoundingClientRect: () => itemRect};
  const tooltip = {style: {}, getBoundingClientRect: () => tipRect};
  widget._kbusPosition(item, tooltip, panel);
  const left = Number.parseFloat(tooltip.style.left);
  const top = Number.parseFloat(tooltip.style.top);
  const safeLeft = Math.max(panelRect.left, 0);
  const safeTop = Math.max(panelRect.top, 0);
  const safeRight = Math.min(panelRect.right, 320);
  const safeBottom = Math.min(panelRect.bottom, 240);
  if (left < safeLeft + 8 || left + tipRect.width > safeRight - 8) {
    throw new Error(name + ': horizontal overflow at ' + left);
  }
  if (top < safeTop + 8 || top + tipRect.height > safeBottom - 8) {
    throw new Error(name + ': vertical overflow at ' + top);
  }
  if (expectedSide === 'right' && left !== itemRect.right + 8) {
    throw new Error(name + ': did not prefer right side');
  }
  if (expectedSide === 'left' && left + tipRect.width !== itemRect.left - 8) {
    throw new Error(name + ': did not fall back to left side');
  }
}

check('right', {left:0, top:0, right:300, bottom:200},
  {left:20, top:40, right:40, bottom:140, width:20, height:100},
  {width:100, height:40}, 'right');
check('left', {left:0, top:0, right:300, bottom:200},
  {left:260, top:40, right:280, bottom:140, width:20, height:100},
  {width:100, height:40}, 'left');
check('center-clamp', {left:0, top:0, right:300, bottom:200},
  {left:145, top:40, right:155, bottom:140, width:10, height:100},
  {width:180, height:40}, 'center');
check('viewport-intersection', {left:-50, top:-20, right:200, bottom:150},
  {left:10, top:20, right:30, bottom:100, width:20, height:80},
  {width:100, height:40}, 'right');

const attributes = {};
const arrowAttributes = {};
const connector = {
  svg: {style: {}},
  line: {setAttribute: (name, value) => { attributes[name] = value; }},
  arrow: {setAttribute: (name, value) => { arrowAttributes[name] = value; }}
};
const connectorItem = {
  isConnected: true,
  getBoundingClientRect: () => ({left:240, top:60, right:260, bottom:140, width:20, height:80})
};
const connectorPanel = {
  isConnected: true,
  getBoundingClientRect: () => ({left:0, top:0, right:300, bottom:200})
};
const connectorTooltip = {
  _wkbConnector: connector,
  getBoundingClientRect: () => ({left:20, top:70, right:120, bottom:130, width:100, height:60})
};
widget._kbusPositionConnector(connectorItem, connectorTooltip, connectorPanel);
if (attributes.x1 !== '120' || attributes.y1 !== '100') {
  throw new Error('connector does not start at nearest tooltip edge: ' + JSON.stringify(attributes));
}
if (attributes.x2 !== '250' || attributes.y2 !== '100') {
  throw new Error('connector does not end at item center: ' + JSON.stringify(attributes));
}
if (!arrowAttributes.points || !arrowAttributes.points.startsWith('250,100 ')) {
  throw new Error('arrow tip does not end at item center: ' + arrowAttributes.points);
}
`
	out, err := exec.Command(node, "-e", script, string(data)).CombinedOutput()
	if err != nil {
		t.Fatalf("tooltip positioning JavaScript: %v\n%s", err, out)
	}
}

// TestWidgetTooltipPinContracts guards the interactive pinned state: one active
// popup, keyboard activation/closing, a safe close button, and focus restoration.
func TestWidgetTooltipPinContracts(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(widgetDir, "assets/js/class.widget.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)

	required := []string{
		"this._kbusActive",
		"e.key === 'Enter'",
		"e.key === ' '",
		"e.key === 'Escape'",
		"document.createElement('button')",
		"btn.textContent = '×'",
		"btn.setAttribute('aria-label', 'Close details')",
		"wago-kbus-tooltip--pinned",
		"tooltip.setAttribute('role', 'dialog')",
		"home.setAttribute('aria-expanded', 'false')",
		"returnFocus.focus()",
	}
	for _, want := range required {
		if !strings.Contains(src, want) {
			t.Errorf("class.widget.js missing pinned-tooltip contract %q", want)
		}
	}
	if strings.Contains(src, "innerHTML") {
		t.Error("class.widget.js must not introduce an innerHTML sink")
	}
}

// TestWidgetTooltipCSSContracts ensures fixed-percentage positioning cannot
// regress and pinned content remains interactive and selectable.
func TestWidgetTooltipCSSContracts(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(widgetDir, "assets/css/widget.css"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)

	for _, forbidden := range []string{"top: 65%", "top:65%", "top: 35%", "top:35%"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("widget.css still contains brittle tooltip placement %q", forbidden)
		}
	}
	for _, want := range []string{
		".wago-kbus-tooltip--floating",
		"position: fixed",
		".wago-kbus-tooltip-connector",
		".wago-kbus-tooltip-connector-line",
		".wago-kbus-tooltip-connector-arrow",
		"z-index: 9998",
		"pointer-events: none",
		".wago-kbus-tooltip--pinned",
		"pointer-events: auto",
		"user-select: text",
		".wago-kbus-tooltip-close",
		// v2.0 state wrappers: warning (ambiguous/unknown/incomplete), fault (global K-bus error).
		".wago-kbus-item--warning",
		".wago-kbus-item--fault",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("widget.css missing tooltip contract %q", want)
		}
	}
}

func TestWidgetTooltipMarkupAndVersion(t *testing.T) {
	view, err := os.ReadFile(filepath.Join(widgetDir, "views/widget.view.php"))
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(view), `tabindex="0" aria-expanded="false"`); count < 2 {
		t.Errorf("widget.view.php has %d keyboard-enabled item render paths, want at least 2", count)
	}

	data, err := os.ReadFile(filepath.Join(widgetDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("manifest.json is not valid JSON: %v", err)
	}
	if manifest["version"] != "1.4.0" {
		t.Errorf("manifest version=%q, want 1.4.0", manifest["version"])
	}
}

// parseYAMLTemplate is a helper shared across widget tests.
func parseYAMLTemplate(data []byte, out *templateDocument) error {
	return yaml.Unmarshal(data, out)
}
