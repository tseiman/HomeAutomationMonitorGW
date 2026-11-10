package config

import (
	"strings"
	"testing"
	"time"
)

func TestDeltaDefaultsStrictKeysAndSafeBounds(t *testing.T) {
	dir := t.TempDir()
	token := writeSecret(t, dir, "token", "delta-test-token-value")
	base := baseYAML([]string{token}, []string{"127.0.0.1"})
	loaded, err := Load(writeConfigFile(t, dir, base), false)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Delta.Heartbeat.Duration != 5*time.Minute || loaded.Delta.RetentionAge.Duration != 30*time.Minute || loaded.Delta.PollFullInterval.Duration != 10*time.Minute || loaded.Delta.MaxEvents != 10000 || loaded.Delta.MaxBytes != 64<<20 {
		t.Fatalf("delta defaults=%+v", loaded.Delta)
	}

	configured := strings.Replace(base, "authentication:", "delta:\n  heartbeat: 1m\n  retention_age: 2h\n  poll_full_interval: 15m\n  max_events: 20\n  max_bytes: 1048576\nauthentication:", 1)
	loaded, err = Load(writeConfigFile(t, dir, configured), false)
	if err != nil || loaded.Delta.Heartbeat.Duration != time.Minute || loaded.Delta.RetentionAge.Duration != 2*time.Hour || loaded.Delta.PollFullInterval.Duration != 15*time.Minute || loaded.Delta.MaxEvents != 20 || loaded.Delta.MaxBytes != 1048576 {
		t.Fatalf("configured delta=%+v err=%v", loaded.Delta, err)
	}

	for name, replacement := range map[string]string{
		"unknown":       "delta:\n  typo: true\n",
		"heartbeat-low": "delta:\n  heartbeat: 4s\n",
		"full-low":      "delta:\n  poll_full_interval: 59s\n",
		"full-high":     "delta:\n  poll_full_interval: 25h\n",
		"age-high":      "delta:\n  retention_age: 25h\n",
		"events-low":    "delta:\n  max_events: 0\n",
		"bytes-low":     "delta:\n  max_bytes: 100\n",
	} {
		t.Run(name, func(t *testing.T) {
			body := replacement + base
			if _, err := Load(writeConfigFile(t, dir, body), false); err == nil {
				t.Fatal("invalid delta settings accepted")
			}
		})
	}
}
