package httpapi

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
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
