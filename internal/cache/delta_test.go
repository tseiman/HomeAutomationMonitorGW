package cache

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/metrics"
)

func metric(identity, name string, value any, at time.Time) metrics.Metric {
	return metrics.Metric{Identity: identity, Name: name, Value: value, ValueType: "mixed", Timestamp: at}
}

func TestDeltaTracksTypedChangesHeartbeatPeakReturnAndRemovals(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := New(func() time.Time { return now }, Options{Heartbeat: 5 * time.Minute, RetentionAge: time.Hour, MaxEvents: 100, MaxBytes: 1 << 20})
	store.Register("generic", "test", time.Minute)
	initial := store.Cursor()

	mixed := map[string]any{"enabled": true, "levels": []any{int64(1), "two", nil}}
	store.Success("generic", []metrics.Metric{metric("key/a", "a", mixed, now)}, time.Millisecond)
	first, err := store.Delta(initial)
	if err != nil || len(first.Transactions) != 1 || len(first.Transactions[0].Metrics) != 1 {
		t.Fatalf("first delta=%+v err=%v", first, err)
	}
	first.Transactions[0].Metrics[0].Metric.Value.(map[string]any)["enabled"] = false
	replayed, err := store.Delta(initial)
	if err != nil || replayed.Transactions[0].Metrics[0].Metric.Value.(map[string]any)["enabled"] != true {
		t.Fatalf("returned transaction mutated journal: %+v err=%v", replayed, err)
	}
	cursor1 := first.NextCursor

	// Mutating caller-owned nested data must not mutate snapshots or journal records.
	mixed["enabled"] = false
	now = now.Add(time.Minute)
	store.Success("generic", []metrics.Metric{metric("key/a", "renamed", map[string]any{"enabled": true, "levels": []any{int64(1), "two", nil}}, now)}, time.Millisecond)
	unchanged, err := store.Delta(cursor1)
	if err != nil || len(unchanged.Transactions) != 1 || len(unchanged.Transactions[0].Metrics) != 1 {
		t.Fatalf("metadata change delta=%+v err=%v", unchanged, err)
	}
	cursor2 := unchanged.NextCursor

	// Timestamp-only changes are suppressed until the configured heartbeat.
	now = now.Add(time.Minute)
	store.Success("generic", []metrics.Metric{metric("key/a", "renamed", map[string]any{"enabled": true, "levels": []any{int64(1), "two", nil}}, now)}, time.Millisecond)
	timestampOnly, _ := store.Delta(cursor2)
	if len(timestampOnly.Transactions) != 1 || len(timestampOnly.Transactions[0].Metrics) != 0 {
		t.Fatalf("timestamp-only change emitted metrics: %+v", timestampOnly)
	}
	cursor3 := timestampOnly.NextCursor

	now = now.Add(5 * time.Minute)
	store.Success("generic", []metrics.Metric{metric("key/a", "renamed", map[string]any{"enabled": true, "levels": []any{int64(1), "two", nil}}, now)}, time.Millisecond)
	heartbeat, _ := store.Delta(cursor3)
	if len(heartbeat.Transactions) != 1 || len(heartbeat.Transactions[0].Metrics) != 1 || !heartbeat.Transactions[0].Metrics[0].Heartbeat {
		t.Fatalf("heartbeat=%+v", heartbeat)
	}

	peakStart := heartbeat.NextCursor
	now = now.Add(time.Second)
	store.Success("generic", []metrics.Metric{metric("key/a", "renamed", float64(2), now)}, time.Millisecond)
	now = now.Add(time.Second)
	store.Success("generic", []metrics.Metric{metric("key/a", "renamed", float64(.45), now)}, time.Millisecond)
	peak, _ := store.Delta(peakStart)
	if len(peak.Transactions) != 2 || peak.Transactions[0].Metrics[0].Metric.Value != float64(2) || peak.Transactions[1].Metrics[0].Metric.Value != float64(.45) {
		t.Fatalf("peak and return not retained: %+v", peak)
	}

	now = now.Add(time.Second)
	beforeRemoval := store.Cursor()
	store.Success("generic", nil, time.Millisecond)
	removed, _ := store.Delta(beforeRemoval)
	if len(removed.Transactions) != 1 || len(removed.Transactions[0].Metrics) != 1 || removed.Transactions[0].Metrics[0].Operation != "remove" || removed.Transactions[0].Metrics[0].Identity != "key/a" {
		t.Fatalf("removal=%+v", removed)
	}
}

func TestDeltaFailuresRecoveryReplayMultipleReadersAndAtomicFullCut(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := New(func() time.Time { return now }, Options{Heartbeat: 5 * time.Minute, RetentionAge: time.Hour, MaxEvents: 100, MaxBytes: 1 << 20})
	store.Register("source", "test", time.Minute)
	store.Success("source", []metrics.Metric{metric("stable", "value", int64(1), now)}, time.Millisecond)
	full := store.Full()
	if full.SchemaVersion != 1 || full.Generation == "" || len(full.Sources) != 1 || full.Sources[0].Metrics[0].Value != int64(1) {
		t.Fatalf("full=%+v", full)
	}

	now = now.Add(time.Second)
	store.Failure("source", errors.New("private details"), time.Second)
	failed, err := store.Delta(full.Cursor)
	if err != nil || len(failed.Transactions) != 1 || failed.Transactions[0].Status.Available || len(failed.Transactions[0].Metrics) != 0 {
		t.Fatalf("failure delta=%+v err=%v", failed, err)
	}
	if snapshot, _ := store.Snapshot("source"); len(snapshot.Metrics) != 1 {
		t.Fatal("failure discarded last-known-good values")
	}

	now = now.Add(time.Second)
	store.Success("source", []metrics.Metric{metric("stable", "value", int64(1), now)}, time.Millisecond)
	recovery, _ := store.Delta(failed.NextCursor)
	if len(recovery.Transactions) != 1 || !recovery.Transactions[0].Status.Available || len(recovery.Transactions[0].Metrics) != 0 {
		t.Fatalf("recovery=%+v", recovery)
	}

	readerA, _ := store.Delta(full.Cursor)
	readerB, _ := store.Delta(full.Cursor)
	if !reflect.DeepEqual(readerA, readerB) || readerA.NextCursor == full.Cursor {
		t.Fatalf("replay differs: A=%+v B=%+v", readerA, readerB)
	}
	empty, _ := store.Delta(readerA.NextCursor)
	if len(empty.Transactions) != 0 || empty.NextCursor != readerA.NextCursor || empty.FromCursor != readerA.NextCursor {
		t.Fatalf("empty replay consumed cursor: %+v", empty)
	}
}

func TestDeltaReadersOwnIndependentCursorsAtDifferentBootstrapPoints(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := New(func() time.Time { return now }, Options{Heartbeat: time.Minute, RetentionAge: time.Hour, MaxEvents: 100, MaxBytes: 1 << 20})
	store.Register("source", "test", time.Minute)
	clientA := store.Full().Cursor

	store.Success("source", []metrics.Metric{metric("key", "value", int64(1), now)}, time.Millisecond)
	clientB := store.Full().Cursor
	now = now.Add(time.Second)
	store.Success("source", []metrics.Metric{metric("key", "value", int64(2), now)}, time.Millisecond)
	now = now.Add(time.Second)
	store.Success("source", []metrics.Metric{metric("key", "value", int64(1), now)}, time.Millisecond)

	aFirst, err := store.Delta(clientA)
	if err != nil || len(aFirst.Transactions) != 3 {
		t.Fatalf("client A replay=%+v err=%v", aFirst, err)
	}
	bFirst, err := store.Delta(clientB)
	if err != nil || len(bFirst.Transactions) != 2 {
		t.Fatalf("client B replay=%+v err=%v", bFirst, err)
	}
	if aFirst.Transactions[1].Metrics[0].Metric.Value != int64(2) || aFirst.Transactions[2].Metrics[0].Metric.Value != int64(1) {
		t.Fatalf("intermediate and return values lost: %+v", aFirst.Transactions)
	}
	aAgain, _ := store.Delta(clientA)
	bAgain, _ := store.Delta(clientB)
	if !reflect.DeepEqual(aFirst, aAgain) || !reflect.DeepEqual(bFirst, bAgain) {
		t.Fatal("one reader consumed or changed another reader's replay")
	}
}

func TestDeltaPaginatesWholeTransactions(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := New(func() time.Time { return now }, Options{Heartbeat: time.Minute, RetentionAge: time.Hour, MaxEvents: 2000, MaxBytes: 8 << 20})
	store.Register("source", "test", time.Minute)
	before := store.Cursor()
	for index := 0; index < deltaPageSize+1; index++ {
		store.Failure("source", errors.New("unavailable"), time.Millisecond)
	}
	first, err := store.Delta(before)
	if err != nil || len(first.Transactions) != deltaPageSize || !first.HasMore || first.NextCursor == first.HeadCursor {
		t.Fatalf("first page transactions=%d has_more=%v err=%v", len(first.Transactions), first.HasMore, err)
	}
	second, err := store.Delta(first.NextCursor)
	if err != nil || len(second.Transactions) != 1 || second.HasMore || second.NextCursor != second.HeadCursor {
		t.Fatalf("second page=%+v err=%v", second, err)
	}
}

func TestDeltaCursorExpiresByAgeWithoutAnotherPoll(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := New(func() time.Time { return now }, Options{Heartbeat: time.Minute, RetentionAge: time.Minute, MaxEvents: 10, MaxBytes: 1 << 20})
	store.Register("source", "test", time.Minute)
	before := store.Cursor()
	store.Success("source", []metrics.Metric{metric("key", "value", 1, now)}, time.Millisecond)
	now = now.Add(2 * time.Minute)
	if _, err := store.Delta(before); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("age-expired cursor error=%v", err)
	}
}

func TestDeltaCursorErrorsGenerationAndWholeTransactionPruning(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := New(func() time.Time { return now }, Options{Heartbeat: time.Minute, RetentionAge: 2 * time.Minute, MaxEvents: 2, MaxBytes: 800})
	store.Register("source", "test", time.Minute)
	origin := store.Cursor()
	for value := int64(1); value <= 3; value++ {
		store.Success("source", []metrics.Metric{metric("key", "value", value, now)}, time.Millisecond)
		now = now.Add(time.Minute)
	}
	if _, err := store.Delta(origin); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("old cursor error=%v", err)
	}
	if _, err := store.Delta("not-a-cursor"); !errors.Is(err, ErrCursorMalformed) {
		t.Fatalf("malformed cursor error=%v", err)
	}
	other := New(func() time.Time { return now })
	if _, err := store.Delta(other.Cursor()); !errors.Is(err, ErrCursorGeneration) {
		t.Fatalf("wrong generation error=%v", err)
	}
	future := store.cursorFor(store.head + 1)
	if _, err := store.Delta(future); !errors.Is(err, ErrCursorFuture) {
		t.Fatalf("future cursor error=%v", err)
	}

	// The newest transaction remains replayable even when it alone exceeds max_bytes.
	big := New(func() time.Time { return now }, Options{Heartbeat: time.Minute, RetentionAge: time.Hour, MaxEvents: 1, MaxBytes: 128})
	big.Register("source", "test", time.Minute)
	before := big.Cursor()
	big.Success("source", []metrics.Metric{metric("key", "value", string(make([]byte, 1024)), now)}, time.Millisecond)
	got, err := big.Delta(before)
	if err != nil || len(got.Transactions) != 1 {
		t.Fatalf("latest oversized transaction was lost: %+v err=%v", got, err)
	}
}

func TestDeltaRejectsAmbiguousMetricIdentitiesWithoutMutatingSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := New(func() time.Time { return now })
	store.Register("source", "test", time.Minute)
	before := store.Cursor()

	for name, values := range map[string][]metrics.Metric{
		"empty":     {{Name: "", Identity: "", Value: 1}},
		"duplicate": {{Name: "first", Identity: "same", Value: 1}, {Name: "second", Identity: "same", Value: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.Success("source", values, time.Millisecond); !errors.Is(err, ErrInvalidMetrics) {
				t.Fatalf("Success error=%v", err)
			}
			if got := store.Cursor(); got != before {
				t.Fatalf("invalid poll advanced cursor: before=%s after=%s", before, got)
			}
			snapshot, _ := store.Snapshot("source")
			if len(snapshot.Metrics) != 0 || snapshot.Available {
				t.Fatalf("invalid poll mutated snapshot: %+v", snapshot)
			}
		})
	}
}

func TestDeltaRejectsNonFiniteValuesWithoutMutatingSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := New(func() time.Time { return now })
	store.Register("source", "test", time.Minute)
	before := store.Cursor()

	err := store.Success("source", []metrics.Metric{{Identity: "load", Name: "load", Value: math.NaN()}}, time.Millisecond)
	if !errors.Is(err, ErrInvalidMetrics) {
		t.Fatalf("Success error=%v", err)
	}
	if got := store.Cursor(); got != before {
		t.Fatalf("invalid poll advanced cursor: before=%s after=%s", before, got)
	}
	snapshot, _ := store.Snapshot("source")
	if len(snapshot.Metrics) != 0 || snapshot.Available {
		t.Fatalf("invalid poll mutated snapshot: %+v", snapshot)
	}
}

func TestDeltaJournalsDiscoveryReplacementAndRemoval(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := New(func() time.Time { return now })
	store.Register("source", "test", time.Minute)
	before := store.Cursor()
	definition := metrics.Definition{Name: "voltage", Type: "number", Unit: "V"}

	store.SetDiscovery("source", []metrics.Definition{definition})
	added, err := store.Delta(before)
	if err != nil || len(added.Transactions) != 1 || !added.Transactions[0].DiscoveryChanged || len(added.Transactions[0].Discovery) != 1 {
		t.Fatalf("discovery add=%+v err=%v", added, err)
	}

	store.SetDiscovery("source", []metrics.Definition{definition})
	unchanged, err := store.Delta(added.NextCursor)
	if err != nil || len(unchanged.Transactions) != 0 {
		t.Fatalf("unchanged discovery emitted delta=%+v err=%v", unchanged, err)
	}

	store.SetDiscovery("source", nil)
	removed, err := store.Delta(added.NextCursor)
	if err != nil || len(removed.Transactions) != 1 || !removed.Transactions[0].DiscoveryChanged || len(removed.Transactions[0].Discovery) != 0 {
		t.Fatalf("discovery removal=%+v err=%v", removed, err)
	}
}

func TestDeltaCursorSyntaxIsCanonicalAndSequenceDoesNotWrap(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := New(func() time.Time { return now })
	store.Register("source", "test", time.Minute)
	parts := strings.Split(store.Cursor(), ".")
	for _, cursor := range []string{
		"v1." + parts[1] + ".+1",
		"v1." + parts[1] + ". 1",
		"v1." + parts[1] + ".01",
	} {
		if _, err := store.Delta(cursor); !errors.Is(err, ErrCursorMalformed) {
			t.Fatalf("noncanonical cursor %q error=%v", cursor, err)
		}
	}

	oldGeneration := store.generation
	store.head = math.MaxUint64
	if err := store.Success("source", []metrics.Metric{metric("key", "value", 1, now)}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if store.generation == oldGeneration || store.head != 1 || len(store.journal) != 1 || store.journal[0].Sequence != 1 {
		t.Fatalf("overflow was not converted to a fresh generation: generation=%s head=%d journal=%+v", store.generation, store.head, store.journal)
	}
}
