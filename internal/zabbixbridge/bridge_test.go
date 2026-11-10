package zabbixbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/cache"
	"github.com/tseiman/HomeAutomationMonitorGW/internal/metrics"
)

func TestStoreGatewayBootstrapsAndReplaysDirectly(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	store := cache.New(func() time.Time { return now })
	store.Register("source", "nut", time.Minute)
	identity := "native:input.voltage"
	if err := store.Success("source", []metrics.Metric{{Identity: identity, Name: "voltage", Value: 24.5, ValueType: "number", Timestamp: now}}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	state := &memoryState{}
	pusher := &recordingPusher{}
	worker := bridge{gateway: storeGateway{store}, zabbix: pusher, state: state, host: "host", batchSize: 100, maxPages: 10}
	if err := worker.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := state.cursor
	if first == "" || len(pusher.batches) < 2 {
		t.Fatalf("bootstrap cursor=%q batches=%d", first, len(pusher.batches))
	}
	wantKey := fmt.Sprintf(`automation.gateway.metric.numeric["source","%s"]`, metricKey(identity))
	if got := pusher.batches[1][0].Key; got != wantKey {
		t.Fatalf("bootstrap key=%q want stable identity key %q", got, wantKey)
	}
	now = now.Add(time.Second)
	if err := store.Success("source", []metrics.Metric{{Identity: identity, Name: "voltage", Value: 25.0, ValueType: "number", Timestamp: now}}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := worker.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.cursor == first {
		t.Fatal("delta cursor did not advance")
	}
	if got := pusher.batches[len(pusher.batches)-1][0].Key; got != wantKey {
		t.Fatalf("delta key=%q want stable identity key %q", got, wantKey)
	}
}

func TestCheckDoesNotCreateMissingCursorParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-created")
	cfg := Config{CursorStatePath: filepath.Join(parent, "cursor")}
	if err := Check(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(parent); !os.IsNotExist(err) {
		t.Fatalf("Check created or touched cursor parent: %v", err)
	}
}

func TestStoreGatewayResynchronizesOnlyGenerationAndExpiredCursors(t *testing.T) {
	store := cache.New(time.Now)
	gateway := storeGateway{store}
	_, code, err := gateway.delta(context.Background(), "malformed")
	if err == nil || code != 0 {
		t.Fatalf("malformed cursor code=%d err=%v", code, err)
	}
	other := cache.New(time.Now).Cursor()
	_, code, err = gateway.delta(context.Background(), other)
	if !errors.Is(err, cache.ErrCursorGeneration) || code != 409 {
		t.Fatalf("generation cursor code=%d err=%v", code, err)
	}
}

func TestDaemonLogsFailureRecoveryAndStopWithoutSecrets(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	g := &recoveringGateway{failures: 1}
	b := bridge{gateway: g, zabbix: &recordingPusher{}, state: &memoryState{}, host: "host", batchSize: 100, maxPages: 10}
	d := &Daemon{cfg: Config{ZabbixAPIURL: "https://zabbix.invalid/api_jsonrpc.php", ZabbixHost: "host", PollInterval: time.Millisecond, InventoryInterval: time.Hour, Token: "zabbix-secret"}, bridge: b, logger: logger}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)
	text := logs.String()
	for _, want := range []string{"zabbix worker starting", "zabbix bridge poll failed", "zabbix bridge poll recovered", "zabbix worker stopped", "https://zabbix.invalid", "host"} {
		if !strings.Contains(text, want) {
			t.Fatalf("logs lack %q: %s", want, text)
		}
	}
	if strings.Contains(text, "zabbix-secret") {
		t.Fatalf("secret leaked: %s", text)
	}
	if strings.Count(text, "poll recovered") != 1 {
		t.Fatalf("recovery logged repeatedly: %s", text)
	}
}

type recoveringGateway struct{ failures int }

func (g *recoveringGateway) full(context.Context) (fullResponse, error) {
	if g.failures > 0 {
		g.failures--
		return fullResponse{}, fmt.Errorf("temporary failure")
	}
	return fullResponse{SchemaVersion: 1, Generation: "AAAAAAAAAAAAAAAAAAAAAA", Cursor: "v1.AAAAAAAAAAAAAAAAAAAAAA.1", CapturedAt: time.Now(), Sources: []snapshot{}}, nil
}
func (g *recoveringGateway) delta(_ context.Context, cursor string) (deltaResponse, int, error) {
	return deltaResponse{SchemaVersion: 1, FromCursor: cursor, NextCursor: cursor, HeadCursor: cursor, Transactions: []transaction{}}, 200, nil
}

type fakeGateway struct {
	fulls                 []fullResponse
	deltas                []deltaResponse
	codes                 []int
	errs                  []error
	fullCalls, deltaCalls int
}

func (f *fakeGateway) full(context.Context) (fullResponse, error) {
	i := f.fullCalls
	f.fullCalls++
	return f.fulls[i], nil
}
func (f *fakeGateway) delta(context.Context, string) (deltaResponse, int, error) {
	i := f.deltaCalls
	f.deltaCalls++
	var e error
	if i < len(f.errs) {
		e = f.errs[i]
	}
	return f.deltas[i], f.codes[i], e
}

type recordingPusher struct {
	batches [][]historyValue
	failAt  int
	calls   int
}

func (p *recordingPusher) push(_ context.Context, v []historyValue) error {
	p.calls++
	cp := append([]historyValue(nil), v...)
	p.batches = append(p.batches, cp)
	if p.failAt == p.calls {
		return errors.New("ambiguous failure")
	}
	return nil
}

type memoryState struct {
	cursor  string
	saves   []string
	saveErr error
}

func (m *memoryState) Load() (string, error) { return m.cursor, nil }
func (m *memoryState) Save(v string) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.cursor = v
	m.saves = append(m.saves, v)
	return nil
}
func raw(s string) json.RawMessage { return json.RawMessage(s) }
func testFull(cursor string) fullResponse {
	return fullResponse{
		SchemaVersion: 1,
		Generation:    "AAAAAAAAAAAAAAAAAAAAAA",
		Cursor:        cursor,
		CapturedAt:    time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
		Sources: []snapshot{{
			Name: "src", Driver: "generic", Available: true,
			LastSuccess: time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC),
			Metrics:     []metric{{Name: "load", Value: raw("0.45"), ValueType: "number", Timestamp: time.Date(2026, 11, 10, 11, 59, 0, 123456789, time.UTC)}},
		}},
	}
}

func TestInventoryRefreshPushesOnlyFullSnapshotAndNeverAdvancesCursor(t *testing.T) {
	oldCursor := "v1.AAAAAAAAAAAAAAAAAAAAAA.1"
	fullCursor := "v1.AAAAAAAAAAAAAAAAAAAAAA.3"
	g := &fakeGateway{fulls: []fullResponse{testFull(fullCursor)}}
	p := &recordingPusher{}
	s := &memoryState{cursor: oldCursor}
	b := bridge{gateway: g, zabbix: p, state: s, host: "host", batchSize: 1, maxPages: 10}

	if err := b.refreshInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.cursor != oldCursor || len(s.saves) != 0 {
		t.Fatalf("inventory refresh advanced cursor: %+v", s)
	}
	if len(p.batches) != 1 || len(p.batches[0]) != 1 || p.batches[0][0].Key != "automation.gateway.snapshot" {
		t.Fatalf("inventory refresh pushed non-snapshot values: %+v", p.batches)
	}
}

func TestBootstrapPushesSnapshotFirstAndPersistsOnlyAfterEveryBatch(t *testing.T) {
	cursor := "v1.AAAAAAAAAAAAAAAAAAAAAA.1"
	g := &fakeGateway{fulls: []fullResponse{testFull(cursor), testFull(cursor)}}
	p := &recordingPusher{failAt: 2}
	s := &memoryState{}
	b := bridge{gateway: g, zabbix: p, state: s, host: "host", batchSize: 1, maxPages: 10}
	if err := b.poll(context.Background()); err == nil {
		t.Fatal("bootstrap succeeded after partial push")
	}
	if s.cursor != "" || len(s.saves) != 0 {
		t.Fatalf("cursor advanced: %+v", s)
	}
	if len(p.batches) < 2 || p.batches[0][0].Key != "automation.gateway.snapshot" {
		t.Fatalf("ordering=%v", p.batches)
	}
	p.failAt = 0
	if err := b.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.cursor != cursor {
		t.Fatalf("cursor=%q", s.cursor)
	}
}

func TestDeltaPreservesIntermediateValuesTimestampsAndRetry(t *testing.T) {
	c1 := "v1.AAAAAAAAAAAAAAAAAAAAAA.1"
	c2 := "v1.AAAAAAAAAAAAAAAAAAAAAA.2"
	c3 := "v1.AAAAAAAAAAAAAAAAAAAAAA.3"
	t1 := time.Date(2026, 11, 10, 12, 0, 0, 123456789, time.UTC)
	t2 := t1.Add(time.Nanosecond)
	status := sourceStatus{Name: "src", Driver: "x", Available: true, LastSuccess: t1}
	d := deltaResponse{SchemaVersion: 1, FromCursor: c1, NextCursor: c3, HeadCursor: c3, Transactions: []transaction{
		{Sequence: 2, Cursor: c2, CapturedAt: t1, Source: "src", Status: status, Metrics: []metricRecord{{Operation: "upsert", Identity: "x", Metric: &metric{Name: "x", Value: raw("2.0"), ValueType: "number", Timestamp: t1}}}},
		{Sequence: 3, Cursor: c3, CapturedAt: t2, Source: "src", Status: status, Metrics: []metricRecord{{Operation: "upsert", Identity: "x", Metric: &metric{Name: "x", Value: raw("0.45"), ValueType: "number", Timestamp: t2}}}},
	}}
	g := &fakeGateway{deltas: []deltaResponse{d, d}, codes: []int{200, 200}}
	p := &recordingPusher{failAt: 1}
	s := &memoryState{cursor: c1}
	b := bridge{gateway: g, zabbix: p, state: s, host: "host", batchSize: 100, maxPages: 10}
	if err := b.poll(context.Background()); err == nil {
		t.Fatal("ambiguous push succeeded")
	}
	if s.cursor != c1 {
		t.Fatal("cursor advanced")
	}
	p.failAt = 0
	if err := b.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.cursor != c3 {
		t.Fatalf("cursor=%q", s.cursor)
	}
	var got []historyValue
	for _, batch := range p.batches {
		for _, v := range batch {
			if v.Key == `automation.gateway.metric.numeric["src","0078"]` {
				got = append(got, v)
			}
		}
	}
	if len(got) != 3 || got[0].Value != "2.0" || got[1].Value != "2.0" || got[2].Value != "0.45" || got[1].Clock != t1.Unix() || got[1].NS != 123456789 || got[2].NS != 123456790 {
		t.Fatalf("values=%+v", got)
	}
}

func TestCursorResetStatusesResyncAndMalformedFailClosed(t *testing.T) {
	for _, code := range []int{409, 410} {
		t.Run(string(rune(code)), func(t *testing.T) {
			c := "v1.AAAAAAAAAAAAAAAAAAAAAA.1"
			next := "v1.BBBBBBBBBBBBBBBBBBBBBB.0"
			resync := testFull(next)
			resync.Generation = "BBBBBBBBBBBBBBBBBBBBBB"
			g := &fakeGateway{deltas: []deltaResponse{{}}, codes: []int{code}, errs: []error{errors.New("reset")}, fulls: []fullResponse{resync}}
			p := &recordingPusher{}
			s := &memoryState{cursor: c}
			b := bridge{gateway: g, zabbix: p, state: s, host: "h", batchSize: 100, maxPages: 2}
			if err := b.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if s.cursor != next || g.fullCalls != 1 {
				t.Fatalf("state=%+v full=%d", s, g.fullCalls)
			}
		})
	}
	g := &fakeGateway{}
	s := &memoryState{cursor: "malformed"}
	b := bridge{gateway: g, zabbix: &recordingPusher{}, state: s, host: "h"}
	if err := b.poll(context.Background()); err == nil || g.deltaCalls != 0 || g.fullCalls != 0 {
		t.Fatal("malformed local cursor did not fail closed")
	}
}

func TestMultiPageEmptyDeltaStatusTombstoneAndUnsupportedFailClosed(t *testing.T) {
	c1 := "v1.AAAAAAAAAAAAAAAAAAAAAA.1"
	c2 := "v1.AAAAAAAAAAAAAAAAAAAAAA.2"
	c3 := "v1.AAAAAAAAAAAAAAAAAAAAAA.3"
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	status := sourceStatus{Name: "src", Available: false, Stale: true, UnavailableSince: at}
	first := deltaResponse{SchemaVersion: 1, FromCursor: c1, NextCursor: c2, HeadCursor: c3, HasMore: true, Transactions: []transaction{{Sequence: 2, Cursor: c2, CapturedAt: at, Source: "src", Status: status, Metrics: []metricRecord{{Operation: "remove", Identity: "old"}}, DiscoveryChanged: true, Discovery: []definition{}}}}
	second := deltaResponse{SchemaVersion: 1, FromCursor: c2, NextCursor: c3, HeadCursor: c3, Transactions: []transaction{{Sequence: 3, Cursor: c3, CapturedAt: at, Source: "src", Status: status, Metrics: []metricRecord{}}}}
	g := &fakeGateway{deltas: []deltaResponse{first, second}, codes: []int{200, 200}}
	p := &recordingPusher{}
	s := &memoryState{cursor: c1}
	b := bridge{gateway: g, zabbix: p, state: s, host: "h", batchSize: 100, maxPages: 3}
	if err := b.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.cursor != c3 || g.deltaCalls != 2 {
		t.Fatalf("state=%+v calls=%d", s, g.deltaCalls)
	}
	for _, batch := range p.batches {
		for _, v := range batch {
			if len(v.Key) >= 26 && v.Key[:26] == "automation.gateway.metric" {
				t.Fatalf("tombstone pushed value: %+v", v)
			}
		}
	}
	boolean := first
	boolean.HasMore = false
	boolean.NextCursor = c2
	boolean.HeadCursor = c2
	boolean.Transactions[0].Metrics = []metricRecord{{Operation: "upsert", Identity: "x", Metric: &metric{Name: "x", Value: raw("true"), ValueType: "boolean", Timestamp: at}}}
	g2 := &fakeGateway{deltas: []deltaResponse{boolean}, codes: []int{200}}
	p2 := &recordingPusher{}
	s2 := &memoryState{cursor: c1}
	if err := (&bridge{gateway: g2, zabbix: p2, state: s2, host: "h", batchSize: 100, maxPages: 2}).poll(context.Background()); err != nil || s2.cursor != c2 {
		t.Fatalf("boolean value was not delivered: cursor=%q err=%v", s2.cursor, err)
	}
	foundBoolean := false
	for _, batch := range p2.batches {
		for _, value := range batch {
			if strings.HasPrefix(value.Key, "automation.gateway.metric.numeric") && value.Value == "1" {
				foundBoolean = true
			}
		}
	}
	if !foundBoolean {
		t.Fatalf("boolean numeric history value missing: %+v", p2.batches)
	}

	malformedBoolean := boolean
	malformedBoolean.Transactions[0].Metrics = []metricRecord{{Operation: "upsert", Identity: "x", Metric: &metric{Name: "x", Value: raw("null"), ValueType: "boolean", Timestamp: at}}}
	g3 := &fakeGateway{deltas: []deltaResponse{malformedBoolean}, codes: []int{200}}
	p3 := &recordingPusher{}
	s3 := &memoryState{cursor: c1}
	if err := (&bridge{gateway: g3, zabbix: p3, state: s3, host: "h", batchSize: 100, maxPages: 2}).poll(context.Background()); err == nil || s3.cursor != c1 || len(p3.batches) != 0 {
		t.Fatal("malformed boolean did not fail closed")
	}

	unknown := boolean
	unknown.Transactions[0].Metrics = []metricRecord{{Operation: "upsert", Identity: "x", Metric: &metric{Name: "x", Value: raw(`{"state":true}`), ValueType: "object", Timestamp: at}}}
	g4 := &fakeGateway{deltas: []deltaResponse{unknown}, codes: []int{200}}
	p4 := &recordingPusher{}
	s4 := &memoryState{cursor: c1}
	if err := (&bridge{gateway: g4, zabbix: p4, state: s4, host: "h", batchSize: 100, maxPages: 2}).poll(context.Background()); err == nil || s4.cursor != c1 || len(p4.batches) != 0 {
		t.Fatal("unsupported value did not fail closed")
	}
}

func TestDeltaRejectsSequenceGap(t *testing.T) {
	c1 := "v1.AAAAAAAAAAAAAAAAAAAAAA.1"
	c3 := "v1.AAAAAAAAAAAAAAAAAAAAAA.3"
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	d := deltaResponse{SchemaVersion: 1, FromCursor: c1, NextCursor: c3, HeadCursor: c3, Transactions: []transaction{{Sequence: 3, Cursor: c3, CapturedAt: at, Source: "src", Status: sourceStatus{Name: "src"}, Metrics: []metricRecord{}}}}
	g := &fakeGateway{deltas: []deltaResponse{d}, codes: []int{200}}
	s := &memoryState{cursor: c1}
	if err := (&bridge{gateway: g, zabbix: &recordingPusher{}, state: s, host: "h", batchSize: 100, maxPages: 2}).poll(context.Background()); err == nil || s.cursor != c1 {
		t.Fatal("sequence gap did not fail closed")
	}
}

func TestMetricKeyUTF16AndFamilySelection(t *testing.T) {
	cases := map[string]string{"1.3.6": "0031002e0033002e0036", "ASCII": "00410053004300490049", "a b[\"]": "006100200062005b0022005d", "😀": "d83dde00"}
	for in, want := range cases {
		if got := metricKey(in); got != want {
			t.Errorf("%q=%q want %q", in, got, want)
		}
	}
	m := metric{Name: "fallback", Labels: map[string]string{"oid": "oid"}, Value: raw("1"), ValueType: "counter", Timestamp: time.Unix(1, 2)}
	v, err := metricHistory("s", "h", m)
	if err != nil {
		t.Fatal(err)
	}
	if v.Key != `automation.gateway.metric.numeric["s","006f00690064"]` {
		t.Fatalf("key=%q", v.Key)
	}
	m.Value = raw(`"abc"`)
	m.ValueType = "hex"
	v, err = metricHistory("s", "h", m)
	if err != nil || v.Value != "abc" || v.Key != `automation.gateway.metric["s","006f00690064"]` {
		t.Fatalf("v=%+v err=%v", v, err)
	}
	_ = reflect.DeepEqual
}
