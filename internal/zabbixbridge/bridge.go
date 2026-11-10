package zabbixbridge

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

var cursorPattern = regexp.MustCompile(`^v1\.[A-Za-z0-9_-]{22}\.(0|[1-9][0-9]*)$`)

type gatewayAPI interface {
	full(context.Context) (fullResponse, error)
	delta(context.Context, string) (deltaResponse, int, error)
}
type historyPusher interface {
	push(context.Context, []historyValue) error
}
type stateFile interface {
	Load() (string, error)
	Save(string) error
}

type bridge struct {
	gateway             gatewayAPI
	zabbix              historyPusher
	state               stateFile
	host                string
	batchSize, maxPages int
}

func (b *bridge) poll(ctx context.Context) error {
	cursor, err := b.state.Load()
	if err != nil {
		return fmt.Errorf("load cursor: %w", err)
	}
	if cursor == "" {
		return b.bootstrap(ctx)
	}
	if !cursorPattern.MatchString(cursor) {
		return fmt.Errorf("stored cursor is malformed")
	}
	for page := 0; page < b.maxPages; page++ {
		delta, code, err := b.gateway.delta(ctx, cursor)
		if err != nil {
			if code == http.StatusConflict || code == http.StatusGone {
				return b.bootstrap(ctx)
			}
			return err
		}
		if err := validateDelta(delta, cursor); err != nil {
			return err
		}
		if len(delta.Transactions) == 0 {
			if delta.HasMore || delta.NextCursor != cursor {
				return fmt.Errorf("delta page made no progress")
			}
			return nil
		}
		for _, tx := range delta.Transactions {
			values, err := b.transactionValues(tx)
			if err != nil {
				return err
			}
			if err := b.pushBatches(ctx, values); err != nil {
				return err
			}
			if err := b.state.Save(tx.Cursor); err != nil {
				return fmt.Errorf("persist cursor: %w", err)
			}
			cursor = tx.Cursor
		}
		if !delta.HasMore {
			return nil
		}
	}
	return fmt.Errorf("delta exceeded max_pages")
}

func (b *bridge) bootstrap(ctx context.Context) error {
	full, err := b.gateway.full(ctx)
	if err != nil {
		return err
	}
	if err := validateFull(full); err != nil {
		return err
	}
	snapshotBody, values, err := b.fullValues(full)
	if err != nil {
		return err
	}
	if err := b.zabbix.push(ctx, []historyValue{{Host: b.host, Key: "automation.gateway.snapshot", Value: string(snapshotBody), Clock: full.CapturedAt.Unix(), NS: full.CapturedAt.Nanosecond()}}); err != nil {
		return err
	}
	if err := b.pushBatches(ctx, values); err != nil {
		return err
	}
	if err := b.state.Save(full.Cursor); err != nil {
		return fmt.Errorf("persist cursor: %w", err)
	}
	return nil
}

// refreshInventory updates only the complete snapshot used by dependent items
// and LLD. It deliberately leaves the replay cursor untouched so transactions
// committed around the snapshot cannot be skipped by the delta consumer.
func (b *bridge) refreshInventory(ctx context.Context) error {
	full, err := b.gateway.full(ctx)
	if err != nil {
		return err
	}
	if err := validateFull(full); err != nil {
		return err
	}
	snapshotBody, _, err := b.fullValues(full)
	if err != nil {
		return err
	}
	return b.zabbix.push(ctx, []historyValue{{Host: b.host, Key: "automation.gateway.snapshot", Value: string(snapshotBody), Clock: full.CapturedAt.Unix(), NS: full.CapturedAt.Nanosecond()}})
}

func (b *bridge) pushBatches(ctx context.Context, values []historyValue) error {
	size := b.batchSize
	if size < 1 {
		size = 100
	}
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		if err := b.zabbix.push(ctx, values[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (b *bridge) fullValues(full fullResponse) ([]byte, []historyValue, error) {
	legacy := make(map[string]snapshot, len(full.Sources))
	values := make([]historyValue, 0)
	for _, source := range full.Sources {
		if source.Name == "" || source.Metrics == nil {
			return nil, nil, fmt.Errorf("invalid full source")
		}
		if _, exists := legacy[source.Name]; exists {
			return nil, nil, fmt.Errorf("duplicate full source %q", source.Name)
		}
		legacy[source.Name] = source
		for _, m := range source.Metrics {
			v, err := metricHistory(source.Name, b.host, m)
			if err != nil {
				return nil, nil, err
			}
			values = append(values, v)
		}
		values = append(values, statusHistory(source.Name, b.host, sourceStatus{Name: source.Name, Driver: source.Driver, Available: source.Available, Stale: source.Stale, LastAttempt: source.LastAttempt, LastSuccess: source.LastSuccess, UnavailableSince: source.UnavailableSince, PollDuration: source.PollDuration, Error: source.Error}, full.CapturedAt, len(source.Metrics) == 0)...)
	}
	body, err := json.Marshal(legacy)
	if err != nil {
		return nil, nil, err
	}
	return body, values, nil
}
func (b *bridge) transactionValues(tx transaction) ([]historyValue, error) {
	values := make([]historyValue, 0, len(tx.Metrics)+5)
	for _, r := range tx.Metrics {
		if r.Identity == "" {
			return nil, fmt.Errorf("metric record has empty identity")
		}
		switch r.Operation {
		case "remove":
			if r.Metric != nil {
				return nil, fmt.Errorf("remove record contains metric")
			}
			continue
		case "upsert":
			if r.Metric == nil {
				return nil, fmt.Errorf("upsert record lacks metric")
			}
			v, err := metricHistory(tx.Source, b.host, *r.Metric)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		default:
			return nil, fmt.Errorf("unsupported metric operation %q", r.Operation)
		}
	}
	values = append(values, statusHistory(tx.Source, b.host, tx.Status, tx.CapturedAt, false)...)
	return values, nil
}

func metricKey(identity string) string {
	units := utf16.Encode([]rune(identity))
	b := make([]byte, len(units)*2)
	for i, u := range units {
		b[2*i] = byte(u >> 8)
		b[2*i+1] = byte(u)
	}
	return hex.EncodeToString(b)
}
func metricHistory(source, host string, m metric) (historyValue, error) {
	if source == "" || m.Name == "" || m.Timestamp.IsZero() {
		return historyValue{}, fmt.Errorf("invalid metric schema")
	}
	identity := m.Name
	if oid := m.Labels["oid"]; oid != "" {
		identity = oid
	}
	family := ""
	value := ""
	switch m.ValueType {
	case "integer", "counter", "number":
		family = "automation.gateway.metric.numeric"
		raw := string(m.Value)
		number, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return historyValue{}, fmt.Errorf("invalid numeric metric %q", m.Name)
		}
		value = raw
	case "boolean":
		family = "automation.gateway.metric.numeric"
		switch string(m.Value) {
		case "true":
			value = "1"
		case "false":
			value = "0"
		default:
			return historyValue{}, fmt.Errorf("invalid boolean metric %q", m.Name)
		}
	case "string", "hex":
		family = "automation.gateway.metric"
		if err := json.Unmarshal(m.Value, &value); err != nil {
			return historyValue{}, fmt.Errorf("invalid text metric %q", m.Name)
		}
	default:
		return historyValue{}, fmt.Errorf("unsupported value type %q", m.ValueType)
	}
	return historyValue{Host: host, Key: fmt.Sprintf(`%s[%q,%q]`, family, source, metricKey(identity)), Value: value, Clock: m.Timestamp.Unix(), NS: m.Timestamp.Nanosecond()}, nil
}
func statusHistory(source, host string, s sourceStatus, at time.Time, empty bool) []historyValue {
	clock, ns := at.Unix(), at.Nanosecond()
	unix := func(v time.Time) string {
		if v.IsZero() {
			return "0"
		}
		return strconv.FormatInt(v.Unix(), 10)
	}
	boolean := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	health := 0
	if s.LastSuccess.IsZero() || s.Stale || empty {
		health = 1
	}
	if !s.Available && !s.LastSuccess.IsZero() {
		health = 2
	}
	pairs := [][2]string{{"available", boolean(s.Available)}, {"stale", boolean(s.Stale)}, {"unavailable_since", unix(s.UnavailableSince)}, {"last_success", unix(s.LastSuccess)}, {"health", strconv.Itoa(health)}}
	out := make([]historyValue, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, historyValue{Host: host, Key: fmt.Sprintf(`automation.gateway.source.%s[%q]`, pair[0], source), Value: pair[1], Clock: clock, NS: ns})
	}
	return out
}

// Daemon owns secure clients, state locking, and the polling lifecycle.
type Daemon struct {
	cfg    Config
	bridge bridge
	store  *cursorStore
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) (*Daemon, error) {
	gr, err := rootsFor(cfg.GatewayCAFile)
	if err != nil {
		return nil, fmt.Errorf("gateway CA: %w", err)
	}
	zr, err := rootsFor(cfg.ZabbixCAFile)
	if err != nil {
		return nil, fmt.Errorf("zabbix CA: %w", err)
	}
	store, err := openCursorStore(cfg.CursorStatePath)
	if err != nil {
		return nil, err
	}
	g := gatewayClient{baseURL: strings.TrimSuffix(cfg.GatewayURL, "/"), token: cfg.gatewayToken, client: newHTTPClient(gr, cfg.RequestTimeout), maxBody: cfg.MaxResponseBytes}
	z := zabbixClient{url: cfg.ZabbixAPIURL, token: cfg.zabbixToken, client: newHTTPClient(zr, cfg.RequestTimeout), maxBody: cfg.MaxResponseBytes}
	return &Daemon{cfg: cfg, store: store, logger: logger, bridge: bridge{gateway: g, zabbix: z, state: store, host: cfg.ZabbixHost, batchSize: cfg.BatchSize, maxPages: cfg.MaxPages}}, nil
}
func (d *Daemon) Close() error { return d.store.Close() }
func (d *Daemon) Run(ctx context.Context) error {
	poll := time.NewTicker(d.cfg.PollInterval)
	defer poll.Stop()
	inventory := time.NewTicker(d.cfg.InventoryInterval)
	defer inventory.Stop()
	if err := d.bridge.poll(ctx); err != nil && d.logger != nil {
		d.logger.Error("zabbix bridge poll failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
			if err := d.bridge.poll(ctx); err != nil && d.logger != nil {
				d.logger.Error("zabbix bridge poll failed", "error", err)
			}
		case <-inventory.C:
			// Drain every replayable transaction before moving the inventory cut.
			if err := d.bridge.poll(ctx); err != nil {
				if d.logger != nil {
					d.logger.Error("zabbix bridge pre-inventory delta poll failed", "error", err)
				}
				continue
			}
			if err := d.bridge.refreshInventory(ctx); err != nil && d.logger != nil {
				d.logger.Error("zabbix bridge inventory refresh failed", "error", err)
			}
		}
	}
}
