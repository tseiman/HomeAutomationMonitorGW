package zabbix

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeltaBridgeTemplateMigration(t *testing.T) {
	data, err := os.ReadFile("template_homeauthmonitorgw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document templateDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	template := document.Export.Templates[0]
	master := template.Items[0]
	if master.UUID != "a70ce834f05e40b3afc07eaf63b44e9c" {
		t.Fatalf("snapshot UUID changed: %q", master.UUID)
	}
	expectedUUIDs := map[string]string{
		`automation.gateway.source.available["{#SOURCE}"]`:                         "73161e1d7ef64caf9a7619927f0e98b8",
		`automation.gateway.source.stale["{#SOURCE}"]`:                             "e58ffbd0227f4a409012c6e5a36ff2cb",
		`automation.gateway.source.unavailable_since["{#SOURCE}"]`:                 "60142c963f2b4a7980c2fc1c93f613da",
		`automation.gateway.source.health["{#SOURCE}"]`:                            "c90c81ede4ab442aa1d58255fe185abb",
		`automation.gateway.source.last_success["{#SOURCE}"]`:                      "9aae70fb921846059e08c2982cfddf06",
		`automation.gateway.metric["{#SOURCE}","{#METRIC_KEY}"]`:                   "12f9b6497de6437dba2009e0142f3e52",
		`automation.gateway.metric.timestamp["{#SOURCE}","{#METRIC_KEY}"]`:         "7f5faf4a75f1429593ac5b5f2d5d0627",
		`automation.gateway.metric.numeric["{#SOURCE}","{#METRIC_KEY}"]`:           "0ace423d3a7b4ce6881b42a2528dadf5",
		`automation.gateway.metric.numeric.timestamp["{#SOURCE}","{#METRIC_KEY}"]`: "7688bc15166d44db9e4e362a73ce6928",
	}
	if master.Key != "automation.gateway.snapshot" || master.Type != "TRAP" || master.AllowedHosts != "{$AUTOMATION_GATEWAY_ALLOWED_HOSTS}" || master.URL != "" {
		t.Fatalf("master=%+v", master)
	}
	macros := map[string]string{}
	for _, m := range template.Macros {
		macros[m.Macro] = m.Value
	}
	if macros["{$AUTOMATION_GATEWAY_ALLOWED_HOSTS}"] != "127.0.0.1" {
		t.Fatalf("macros=%v", macros)
	}
	for _, retired := range []string{"{$AUTOMATION_GATEWAY_URL}", "{$AUTOMATION_GATEWAY_TOKEN}", "{$AUTOMATION_GATEWAY_TIMEOUT}", "{$AUTOMATION_GATEWAY_INTERVAL}"} {
		if _, ok := macros[retired]; ok {
			t.Fatalf("retired HTTP/bridge macro remains: %s", retired)
		}
	}
	if strings.Contains(string(data), "external bridge") || strings.Contains(string(data), "remote bridge") {
		t.Fatal("template still describes the removed external bridge")
	}
	for _, rule := range template.DiscoveryRules {
		if rule.Type != "DEPENDENT" || rule.MasterItem.Key != "automation.gateway.snapshot" {
			t.Fatalf("discovery not snapshot dependent: %+v", rule)
		}
		for _, p := range rule.ItemPrototypes {
			if want, ok := expectedUUIDs[p.Key]; ok {
				if p.UUID != want {
					t.Fatalf("prototype %q UUID=%q want %q", p.Key, p.UUID, want)
				}
				delete(expectedUUIDs, p.Key)
			}
			isSource := strings.HasPrefix(p.Key, "automation.gateway.source.")
			isValue := strings.HasPrefix(p.Key, "automation.gateway.metric[") || strings.HasPrefix(p.Key, "automation.gateway.metric.numeric[")
			isTimestamp := strings.Contains(p.Key, ".timestamp[")
			if isTimestamp {
				if p.Status != "DISABLED" {
					t.Fatalf("timestamp prototype enabled: %+v", p)
				}
				continue
			}
			if isSource || isValue {
				if p.Type != "TRAP" || p.AllowedHosts != "{$AUTOMATION_GATEWAY_ALLOWED_HOSTS}" || p.MasterItem.Key != "" {
					t.Fatalf("prototype not trapper: %+v", p)
				}
				for _, step := range p.Preprocessing {
					if step.Type == "JSONPATH" {
						t.Fatalf("trapper retains JSONPath: %+v", p)
					}
				}
			}
			if strings.HasPrefix(p.Key, "automation.gateway.metric.numeric[") {
				if len(p.Preprocessing) != 1 || p.Preprocessing[0].Type != "MULTIPLIER" {
					t.Fatalf("numeric multiplier lost: %+v", p)
				}
			}
			if strings.HasPrefix(p.Key, "automation.gateway.metric[") && len(p.Preprocessing) != 0 {
				t.Fatalf("text trap preprocessing=%+v", p.Preprocessing)
			}
		}
		for _, step := range rule.Preprocessing {
			if strings.Contains(strings.Join(step.Parameters, "\n"), "/metrics/delta") {
				t.Fatalf("delta JSONPath in LLD: %+v", step)
			}
		}
	}
	if strings.Count(string(data), "typeof metric.identity === 'string' && metric.identity !== ''") < 2 || strings.Count(string(data), "@.identity == ") < 2 {
		t.Fatal("metric discovery does not prefer the gateway stable identity")
	}
	if !strings.Contains(string(data), "metric.value_type !== 'boolean'") {
		t.Fatal("numeric discovery excludes supported boolean metrics")
	}
	if len(expectedUUIDs) != 0 {
		t.Fatalf("missing preserved prototypes: %v", expectedUUIDs)
	}
}
