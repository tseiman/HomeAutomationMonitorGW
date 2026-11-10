package zabbixbridge

import "time"

// Config contains the already validated, secret-bearing worker settings supplied
// by the gateway configuration loader. It is never serialized or logged.
type Config struct {
	ZabbixAPIURL      string
	ZabbixCAFile      string
	ZabbixHost        string
	PollInterval      time.Duration
	InventoryInterval time.Duration
	RequestTimeout    time.Duration
	CursorStatePath   string
	BatchSize         int
	MaxPages          int
	MaxResponseBytes  int64
	Token             string
}
