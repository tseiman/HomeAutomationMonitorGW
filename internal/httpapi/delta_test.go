package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/cache"
	"github.com/tseiman/HomeAutomationMonitorGW/internal/metrics"
)

func deltaHandler(store *cache.Store) http.Handler {
	digest := sha256.Sum256([]byte("delta-api-token-value"))
	return New(func() View {
		return View{Store: store, TokenDigests: [][sha256.Size]byte{digest}, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
	}, Options{})
}

func pollRequest(t *testing.T, handler http.Handler, token string) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+token))
	if recorder.Code != http.StatusOK {
		t.Fatalf("poll status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

type blockingResponseWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
}

func (w *blockingResponseWriter) Header() http.Header { return w.header }
func (w *blockingResponseWriter) WriteHeader(int)     {}
func (w *blockingResponseWriter) Write(body []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return len(body), nil
}
func (w *blockingResponseWriter) FlushError() error { return nil }

type failingResponseWriter struct {
	header   http.Header
	writeErr error
	flushErr error
	body     bytes.Buffer
}

func (w *failingResponseWriter) Header() http.Header { return w.header }
func (w *failingResponseWriter) WriteHeader(int)     {}
func (w *failingResponseWriter) Write(body []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.body.Write(body)
}
func (w *failingResponseWriter) FlushError() error { return w.flushErr }

func TestTokenPollFailedInitialResponseDoesNotAdvanceState(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now })
	store.Register("source", "test", time.Minute)
	token := "failed-initial-reader-token"
	digest := sha256.Sum256([]byte(token))
	handler := New(func() View {
		return View{Store: store, TokenDigests: [][sha256.Size]byte{digest}, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})

	failed := &failingResponseWriter{header: make(http.Header), writeErr: errors.New("write failed")}
	handler.ServeHTTP(failed, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+token))
	got := pollRequest(t, handler, token)
	if got["mode"] != "full" || got["reason"] != "first_request" {
		t.Fatalf("retry after failed initial response=%v", got)
	}
}

func TestTokenPollFailedSparseFlushReplaysDelta(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now })
	store.Register("source", "test", time.Minute)
	_ = store.Success("source", []metrics.Metric{{Identity: "voltage", Name: "voltage", Value: 1, ValueType: "number", Timestamp: now}}, time.Millisecond)
	token := "failed-sparse-reader-token"
	digest := sha256.Sum256([]byte(token))
	handler := New(func() View {
		return View{Store: store, TokenDigests: [][sha256.Size]byte{digest}, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})
	_ = pollRequest(t, handler, token)

	now = now.Add(time.Second)
	_ = store.Success("source", []metrics.Metric{{Identity: "voltage", Name: "voltage", Value: 2, ValueType: "number", Timestamp: now}}, time.Millisecond)
	failed := &failingResponseWriter{header: make(http.Header), flushErr: errors.New("flush failed")}
	handler.ServeHTTP(failed, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+token))
	got := pollRequest(t, handler, token)
	encoded, _ := json.Marshal(got)
	if got["mode"] != "sparse" || !bytes.Contains(encoded, []byte(`"value":2`)) {
		t.Fatalf("retry after failed sparse flush=%s", encoded)
	}
}

func TestTokenPollFullSparseLatestCollapseRemovalAndIndependentReaders(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now }, cache.Options{Heartbeat: time.Minute, RetentionAge: time.Hour, MaxEvents: 100, MaxBytes: 1 << 20})
	store.Register("source", "test", time.Minute)
	_ = store.Success("source", []metrics.Metric{{Identity: "voltage", Name: "voltage", Value: 1, ValueType: "number", Timestamp: now}}, time.Millisecond)
	tokens := []string{"poll-reader-token-one", "poll-reader-token-two"}
	digests := [][sha256.Size]byte{sha256.Sum256([]byte(tokens[0])), sha256.Sum256([]byte(tokens[1]))}
	api := New(func() View {
		return View{Store: store, TokenDigests: digests, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})

	if got := pollRequest(t, api, tokens[0]); got["mode"] != "full" {
		t.Fatalf("first poll=%v", got)
	}
	if got := pollRequest(t, api, tokens[1]); got["mode"] != "full" {
		t.Fatalf("independent first poll=%v", got)
	}
	now = now.Add(time.Second)
	_ = store.Success("source", []metrics.Metric{{Identity: "voltage", Name: "voltage", Value: 2, ValueType: "number", Timestamp: now}}, time.Millisecond)
	now = now.Add(time.Second)
	_ = store.Success("source", []metrics.Metric{{Identity: "voltage", Name: "voltage", Value: 3, ValueType: "number", Timestamp: now}}, time.Millisecond)

	sparse := pollRequest(t, api, tokens[0])
	encoded, _ := json.Marshal(sparse)
	if sparse["mode"] != "sparse" || strings.Contains(string(encoded), `"value":2`) || !strings.Contains(string(encoded), `"value":3`) {
		t.Fatalf("latest-state sparse=%s", encoded)
	}
	empty := pollRequest(t, api, tokens[0])
	if empty["mode"] != "sparse" || len(empty["sources"].([]any)) != 0 {
		t.Fatalf("empty sparse=%v", empty)
	}

	now = now.Add(time.Minute)
	_ = store.Success("source", []metrics.Metric{{Identity: "voltage", Name: "voltage", Value: 3, ValueType: "number", Timestamp: now}}, time.Millisecond)
	heartbeat := pollRequest(t, api, tokens[0])
	encoded, _ = json.Marshal(heartbeat)
	if !strings.Contains(string(encoded), `"heartbeat":true`) {
		t.Fatalf("heartbeat sparse=%s", encoded)
	}

	now = now.Add(time.Second)
	_ = store.Success("source", nil, time.Millisecond)
	removed := pollRequest(t, api, tokens[0])
	encoded, _ = json.Marshal(removed)
	if removed["mode"] != "full" || removed["reason"] != "metric_removed" || strings.Contains(string(encoded), `"name":"voltage"`) {
		t.Fatalf("removal full=%s", encoded)
	}
}

func TestTokenPollSparseCarriesOnlyChangedMetrics(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now }, cache.Options{Heartbeat: time.Hour, RetentionAge: time.Hour, MaxEvents: 100, MaxBytes: 4 << 20})
	store.Register("source", "test", time.Minute)
	metricsBefore := make([]metrics.Metric, 500)
	for index := range metricsBefore {
		name := fmt.Sprintf("metric-%03d", index)
		metricsBefore[index] = metrics.Metric{Identity: name, Name: name, Value: index, ValueType: "number", Timestamp: now}
	}
	_ = store.Success("source", metricsBefore, time.Millisecond)
	token := "bounded-sparse-reader-token"
	digest := sha256.Sum256([]byte(token))
	handler := New(func() View {
		return View{Store: store, TokenDigests: [][sha256.Size]byte{digest}, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})
	_ = pollRequest(t, handler, token)

	now = now.Add(time.Second)
	metricsAfter := append([]metrics.Metric(nil), metricsBefore...)
	metricsAfter[321].Value = 9999
	metricsAfter[321].Timestamp = now
	_ = store.Success("source", metricsAfter, time.Millisecond)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+token))
	if recorder.Code != http.StatusOK {
		t.Fatalf("poll status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	if bytes.Contains(body, []byte(`"state"`)) || bytes.Contains(body, []byte("metric-320")) || !bytes.Contains(body, []byte("metric-321")) {
		t.Fatalf("sparse payload contains full state or misses change: %s", body)
	}
	if len(body) >= 2048 {
		t.Fatalf("one-change sparse payload is %d bytes for 500 cached metrics", len(body))
	}

	empty := httptest.NewRecorder()
	handler.ServeHTTP(empty, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+token))
	if empty.Code != http.StatusOK || bytes.Contains(empty.Body.Bytes(), []byte(`"state"`)) || !bytes.Contains(empty.Body.Bytes(), []byte(`"sources":[]`)) {
		t.Fatalf("empty sparse payload=%s status=%d", empty.Body.String(), empty.Code)
	}
	if empty.Body.Len() >= 512 {
		t.Fatalf("empty sparse payload is %d bytes for 500 cached metrics", empty.Body.Len())
	}
}

func TestTokenPollNewMetricForcesImmediateFull(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now }, cache.Options{Heartbeat: time.Hour, RetentionAge: time.Hour, MaxEvents: 100, MaxBytes: 1 << 20})
	store.Register("source", "test", time.Minute)
	_ = store.Success("source", []metrics.Metric{{Identity: "existing", Name: "existing", Value: 1, ValueType: "number", Timestamp: now}}, time.Millisecond)
	token := "new-metric-reader-token"
	digest := sha256.Sum256([]byte(token))
	handler := New(func() View {
		return View{Store: store, TokenDigests: [][sha256.Size]byte{digest}, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})
	_ = pollRequest(t, handler, token)

	now = now.Add(time.Second)
	_ = store.Success("source", []metrics.Metric{
		{Identity: "existing", Name: "existing", Value: 1, ValueType: "number", Timestamp: now},
		{Identity: "new", Name: "new", Value: 2, ValueType: "number", Timestamp: now},
	}, time.Millisecond)
	got := pollRequest(t, handler, token)
	if got["mode"] != "full" || got["reason"] != "metric_added" {
		t.Fatalf("new metric response=%v", got)
	}
}

func TestTokenPollFailedMetricAddedFullDoesNotUpdateBaseline(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now })
	store.Register("source", "test", time.Minute)
	_ = store.Success("source", []metrics.Metric{{Identity: "existing", Name: "existing", Value: 1, ValueType: "number", Timestamp: now}}, time.Millisecond)
	token := "failed-new-metric-reader-token"
	digest := sha256.Sum256([]byte(token))
	handler := New(func() View {
		return View{Store: store, TokenDigests: [][sha256.Size]byte{digest}, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})
	_ = pollRequest(t, handler, token)

	now = now.Add(time.Second)
	_ = store.Success("source", []metrics.Metric{
		{Identity: "existing", Name: "existing", Value: 1, ValueType: "number", Timestamp: now},
		{Identity: "new", Name: "new", Value: 2, ValueType: "number", Timestamp: now},
	}, time.Millisecond)
	failed := &failingResponseWriter{header: make(http.Header), writeErr: errors.New("write failed")}
	handler.ServeHTTP(failed, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+token))
	got := pollRequest(t, handler, token)
	if got["mode"] != "full" || got["reason"] != "metric_added" {
		t.Fatalf("retry after failed metric-added full=%v", got)
	}
}

func TestTokenPollMetricBaselineIsIsolatedPerToken(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now })
	store.Register("source", "test", time.Minute)
	_ = store.Success("source", []metrics.Metric{{Identity: "existing", Name: "existing", Value: 1, ValueType: "number", Timestamp: now}}, time.Millisecond)
	tokens := []string{"baseline-token-a", "baseline-token-b"}
	digests := [][sha256.Size]byte{sha256.Sum256([]byte(tokens[0])), sha256.Sum256([]byte(tokens[1]))}
	handler := New(func() View {
		return View{Store: store, TokenDigests: digests, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})
	_ = pollRequest(t, handler, tokens[0])
	_ = pollRequest(t, handler, tokens[1])

	now = now.Add(time.Second)
	_ = store.Success("source", []metrics.Metric{
		{Identity: "existing", Name: "existing", Value: 1, ValueType: "number", Timestamp: now},
		{Identity: "new", Name: "new", Value: 2, ValueType: "number", Timestamp: now},
	}, time.Millisecond)
	failed := &failingResponseWriter{header: make(http.Header), writeErr: errors.New("write failed")}
	handler.ServeHTTP(failed, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+tokens[0]))
	if got := pollRequest(t, handler, tokens[1]); got["mode"] != "full" || got["reason"] != "metric_added" {
		t.Fatalf("token B metric-added full=%v", got)
	}
	if got := pollRequest(t, handler, tokens[0]); got["mode"] != "full" || got["reason"] != "metric_added" {
		t.Fatalf("token A retry after token B commit=%v", got)
	}
}

func TestTokenPollForcesFullOnIntervalDiscoveryAndGeneration(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now }, cache.Options{Heartbeat: time.Minute, RetentionAge: time.Minute, MaxEvents: 2, MaxBytes: 1 << 20})
	store.Register("source", "test", time.Minute)
	token := "poll-reader-token-value"
	digest := sha256.Sum256([]byte(token))
	current := store
	api := New(func() View {
		return View{Store: current, TokenDigests: [][sha256.Size]byte{digest}, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})

	_ = pollRequest(t, api, token)
	now = now.Add(10 * time.Minute)
	if got := pollRequest(t, api, token); got["mode"] != "full" || got["reason"] != "periodic" {
		t.Fatalf("periodic full=%v", got)
	}
	store.SetDiscovery("source", []metrics.Definition{{Name: "new", Type: "number"}})
	if got := pollRequest(t, api, token); got["mode"] != "full" || got["reason"] != "discovery_changed" {
		t.Fatalf("discovery full=%v", got)
	}

	current = cache.New(func() time.Time { return now })
	current.Register("source", "test", time.Minute)
	if got := pollRequest(t, api, token); got["mode"] != "full" || got["reason"] != "reset_required" {
		t.Fatalf("generation full=%v", got)
	}
}

func TestTokenPollExpiryRotationAndConcurrentRequests(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now }, cache.Options{Heartbeat: time.Minute, RetentionAge: time.Minute, MaxEvents: 1, MaxBytes: 1 << 20})
	store.Register("source", "test", time.Minute)
	tokens := []string{"rotating-reader-token-one", "rotating-reader-token-two"}
	digests := [][sha256.Size]byte{sha256.Sum256([]byte(tokens[0])), sha256.Sum256([]byte(tokens[1]))}
	active := append([][sha256.Size]byte(nil), digests...)
	handler := New(func() View {
		return View{Store: store, TokenDigests: active, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})
	first := pollRequest(t, handler, tokens[0])
	encoded, _ := json.Marshal(first)
	if strings.Contains(string(encoded), tokens[0]) {
		t.Fatal("raw token leaked in poll response")
	}

	now = now.Add(2 * time.Minute)
	_ = store.Success("source", []metrics.Metric{{Identity: "x", Name: "x", Value: 1, ValueType: "number", Timestamp: now}}, time.Millisecond)
	_ = store.Success("source", []metrics.Metric{{Identity: "x", Name: "x", Value: 2, ValueType: "number", Timestamp: now}}, time.Millisecond)
	if got := pollRequest(t, handler, tokens[0]); got["mode"] != "full" || got["reason"] != "reset_required" {
		t.Fatalf("expired cursor full=%v", got)
	}

	active = active[1:]
	_ = pollRequest(t, handler, tokens[1])
	active = digests
	if got := pollRequest(t, handler, tokens[0]); got["mode"] != "full" || got["reason"] != "first_request" {
		t.Fatalf("restored token retained removed state=%v", got)
	}

	var wait sync.WaitGroup
	errors := make(chan string, 16)
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+tokens[0]))
			if recorder.Code != http.StatusOK {
				errors <- recorder.Body.String()
			}
		}()
	}
	wait.Wait()
	close(errors)
	for message := range errors {
		t.Fatalf("concurrent poll failed: %s", message)
	}
}

func TestTokenPollSlowWriterDoesNotBlockAnotherToken(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now })
	store.Register("source", "test", time.Minute)
	tokens := []string{"slow-writer-token-a", "fast-writer-token-b"}
	digests := [][sha256.Size]byte{sha256.Sum256([]byte(tokens[0])), sha256.Sum256([]byte(tokens[1]))}
	handler := New(func() View {
		return View{Store: store, TokenDigests: digests, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})
	_ = pollRequest(t, handler, tokens[0])
	_ = pollRequest(t, handler, tokens[1])

	writer := &blockingResponseWriter{header: make(http.Header), entered: make(chan struct{}, 1), release: make(chan struct{})}
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		handler.ServeHTTP(writer, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+tokens[0]))
	}()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("slow token did not reach response write")
	}

	fastDone := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+tokens[1]))
		fastDone <- recorder.Code
	}()
	select {
	case code := <-fastDone:
		if code != http.StatusOK {
			t.Fatalf("fast token status=%d", code)
		}
	case <-time.After(500 * time.Millisecond):
		close(writer.release)
		<-slowDone
		t.Fatal("slow token response write blocked another token")
	}
	close(writer.release)
	<-slowDone
}

func TestTokenPollSlowWriterDoesNotBlockSameTokenOrSkipDelta(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now })
	store.Register("source", "test", time.Minute)
	_ = store.Success("source", []metrics.Metric{{Identity: "x", Name: "x", Value: 1, ValueType: "number", Timestamp: now}}, time.Millisecond)
	token := "same-token-concurrent-reader"
	digest := sha256.Sum256([]byte(token))
	handler := New(func() View {
		return View{Store: store, TokenDigests: [][sha256.Size]byte{digest}, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})
	_ = pollRequest(t, handler, token)
	now = now.Add(time.Second)
	_ = store.Success("source", []metrics.Metric{{Identity: "x", Name: "x", Value: 2, ValueType: "number", Timestamp: now}}, time.Millisecond)

	writer := &blockingResponseWriter{header: make(http.Header), entered: make(chan struct{}, 1), release: make(chan struct{})}
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		handler.ServeHTTP(writer, request(http.MethodGet, "/api/v1/metrics/poll", "Bearer "+token))
	}()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("slow same-token request did not reach response write")
	}

	duplicateDone := make(chan map[string]any, 1)
	go func() { duplicateDone <- pollRequest(t, handler, token) }()
	select {
	case duplicate := <-duplicateDone:
		encoded, _ := json.Marshal(duplicate)
		if duplicate["mode"] != "sparse" || !bytes.Contains(encoded, []byte(`"value":2`)) {
			t.Fatalf("concurrent duplicate=%s", encoded)
		}
	case <-time.After(500 * time.Millisecond):
		close(writer.release)
		<-slowDone
		t.Fatal("slow response write blocked the same token")
	}
	close(writer.release)
	<-slowDone
	if got := pollRequest(t, handler, token); got["mode"] != "sparse" || len(got["sources"].([]any)) != 0 {
		t.Fatalf("concurrent commit skipped or replayed progress=%v", got)
	}
}

func TestTokenPollFollowsAllDeltaPagesAndKeepsLatestValue(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now }, cache.Options{Heartbeat: time.Hour, RetentionAge: time.Hour, MaxEvents: 2000, MaxBytes: 16 << 20})
	store.Register("source", "test", time.Minute)
	_ = store.Success("source", []metrics.Metric{{Identity: "x", Name: "x", Value: 0, ValueType: "number", Timestamp: now}}, time.Millisecond)
	token := "paginated-reader-token"
	digest := sha256.Sum256([]byte(token))
	handler := New(func() View {
		return View{Store: store, TokenDigests: [][sha256.Size]byte{digest}, AllowedClients: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, PollFullInterval: 10 * time.Minute}
	}, Options{Now: func() time.Time { return now }})
	_ = pollRequest(t, handler, token)
	for value := 1; value <= 1001; value++ {
		now = now.Add(time.Millisecond)
		_ = store.Success("source", []metrics.Metric{{Identity: "x", Name: "x", Value: value, ValueType: "number", Timestamp: now}}, time.Millisecond)
	}
	result := pollRequest(t, handler, token)
	encoded, _ := json.Marshal(result)
	if result["mode"] != "sparse" || !strings.Contains(string(encoded), `"value":1001`) || strings.Contains(string(encoded), `"value":1000`) {
		t.Fatalf("paginated latest state=%s", encoded)
	}
}

func TestDeltaAPIFullReplayErrorsAndLegacyShape(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now })
	store.Register("generic", "test", time.Minute)
	store.Success("generic", []metrics.Metric{{Identity: "protocol/key", Name: "display", Value: true, ValueType: "boolean", Timestamp: now}}, time.Millisecond)
	handler := deltaHandler(store)
	headBeforeRequests := store.Cursor()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, request(http.MethodGet, "/api/v1/metrics/all", ""))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized full snapshot status=%d", unauthorized.Code)
	}
	disallowedRequest := request(http.MethodGet, "/api/v1/metrics/delta?since="+headBeforeRequests, "Bearer delta-api-token-value")
	disallowedRequest.RemoteAddr = "198.51.100.25:32100"
	disallowed := httptest.NewRecorder()
	handler.ServeHTTP(disallowed, disallowedRequest)
	if disallowed.Code != http.StatusForbidden {
		t.Fatalf("disallowed delta status=%d", disallowed.Code)
	}

	fullRecorder := httptest.NewRecorder()
	handler.ServeHTTP(fullRecorder, request(http.MethodGet, "/api/v1/metrics/all", "Bearer delta-api-token-value"))
	if fullRecorder.Code != http.StatusOK {
		t.Fatalf("full status=%d body=%s", fullRecorder.Code, fullRecorder.Body.String())
	}
	var full cache.FullResponse
	if err := json.Unmarshal(fullRecorder.Body.Bytes(), &full); err != nil || full.SchemaVersion != 1 || full.Cursor == "" || len(full.Sources) != 1 {
		t.Fatalf("full=%+v err=%v", full, err)
	}

	deltaRecorder := httptest.NewRecorder()
	handler.ServeHTTP(deltaRecorder, request(http.MethodGet, "/api/v1/metrics/delta?since="+full.Cursor, "Bearer delta-api-token-value"))
	if deltaRecorder.Code != http.StatusOK {
		t.Fatalf("delta status=%d body=%s", deltaRecorder.Code, deltaRecorder.Body.String())
	}
	if got := store.Cursor(); got != headBeforeRequests {
		t.Fatalf("HTTP cache reads changed journal head: before=%s after=%s", headBeforeRequests, got)
	}

	legacy := httptest.NewRecorder()
	handler.ServeHTTP(legacy, request(http.MethodGet, "/api/v1/metrics", "Bearer delta-api-token-value"))
	if legacy.Code != http.StatusOK || strings.Contains(legacy.Body.String(), "identity") || strings.Contains(legacy.Body.String(), "schema_version") {
		t.Fatalf("legacy API shape changed: %s", legacy.Body.String())
	}

	cases := []struct {
		query   string
		code    int
		message string
	}{
		{"", 400, "since is required"},
		{"?since=bad", 400, "invalid cursor"},
		{"?since=" + cache.New(time.Now).Cursor(), 409, "reset_required"},
		{"?since=" + store.Cursor() + "&since=" + store.Cursor(), 400, "since must appear once"},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request(http.MethodGet, "/api/v1/metrics/delta"+tc.query, "Bearer delta-api-token-value"))
		if recorder.Code != tc.code || !strings.Contains(recorder.Body.String(), tc.message) {
			t.Fatalf("query=%q status=%d body=%s", tc.query, recorder.Code, recorder.Body.String())
		}
	}
}

func TestDeltaAPIExpiredAndFutureCursorStatuses(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store := cache.New(func() time.Time { return now }, cache.Options{Heartbeat: time.Minute, RetentionAge: time.Hour, MaxEvents: 1, MaxBytes: 1 << 20})
	store.Register("generic", "test", time.Minute)
	old := store.Cursor()
	store.Success("generic", []metrics.Metric{{Identity: "key", Name: "x", Value: 1}}, time.Millisecond)
	store.Success("generic", []metrics.Metric{{Identity: "key", Name: "x", Value: 2}}, time.Millisecond)
	handler := deltaHandler(store)

	for _, tc := range []struct {
		cursor string
		code   int
		text   string
	}{
		{old, http.StatusGone, "cursor expired"},
		{strings.TrimSuffix(store.Cursor(), ".2") + ".999", http.StatusConflict, "cursor is in the future"},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request(http.MethodGet, "/api/v1/metrics/delta?since="+tc.cursor, "Bearer delta-api-token-value"))
		if recorder.Code != tc.code || !strings.Contains(recorder.Body.String(), tc.text) {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}
