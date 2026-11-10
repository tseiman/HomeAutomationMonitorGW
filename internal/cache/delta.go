package cache

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/metrics"
)

const deltaPageSize = 1000

var (
	ErrCursorMalformed  = errors.New("malformed cursor")
	ErrCursorGeneration = errors.New("cursor generation does not match this runtime")
	ErrCursorExpired    = errors.New("cursor has expired")
	ErrCursorFuture     = errors.New("cursor is ahead of the journal")
	ErrInvalidMetrics   = errors.New("poll contains invalid metric identities")
)

// Options bounds the in-memory replay journal and metric heartbeats.
type Options struct {
	Heartbeat    time.Duration
	RetentionAge time.Duration
	MaxEvents    int
	MaxBytes     int64
}

func defaultOptions() Options {
	return Options{Heartbeat: 5 * time.Minute, RetentionAge: 30 * time.Minute, MaxEvents: 10000, MaxBytes: 64 << 20}
}

func (o Options) withDefaults(supplied Options) Options {
	defaults := o
	if supplied.Heartbeat > 0 {
		defaults.Heartbeat = supplied.Heartbeat
	}
	if supplied.RetentionAge > 0 {
		defaults.RetentionAge = supplied.RetentionAge
	}
	if supplied.MaxEvents > 0 {
		defaults.MaxEvents = supplied.MaxEvents
	}
	if supplied.MaxBytes > 0 {
		defaults.MaxBytes = supplied.MaxBytes
	}
	return defaults
}

// SourceStatus is the complete source status captured in one poll transaction.
type SourceStatus struct {
	Name             string        `json:"name"`
	Driver           string        `json:"driver"`
	Available        bool          `json:"available"`
	Stale            bool          `json:"stale"`
	LastAttempt      time.Time     `json:"last_attempt,omitempty"`
	LastSuccess      time.Time     `json:"last_success,omitempty"`
	UnavailableSince time.Time     `json:"unavailable_since"`
	PollDuration     time.Duration `json:"poll_duration_ns"`
	Error            string        `json:"error,omitempty"`
}

// MetricRecord is an upsert, heartbeat, or explicit removal keyed by stable identity.
type MetricRecord struct {
	Operation string          `json:"operation"`
	Identity  string          `json:"identity"`
	Heartbeat bool            `json:"heartbeat,omitempty"`
	Metric    *metrics.Metric `json:"metric,omitempty"`
}

// Transaction atomically describes one completed source poll.
type Transaction struct {
	Sequence         uint64               `json:"sequence"`
	Cursor           string               `json:"cursor"`
	CapturedAt       time.Time            `json:"captured_at"`
	Source           string               `json:"source"`
	Status           SourceStatus         `json:"status"`
	Metrics          []MetricRecord       `json:"metrics"`
	DiscoveryChanged bool                 `json:"discovery_changed,omitempty"`
	Discovery        []metrics.Definition `json:"discovery,omitempty"`
	byteSize         int64
}

// FullResponse is an atomic complete cache snapshot and its matching replay cursor.
type FullResponse struct {
	SchemaVersion int        `json:"schema_version"`
	Generation    string     `json:"generation"`
	Cursor        string     `json:"cursor"`
	CapturedAt    time.Time  `json:"captured_at"`
	Sources       []Snapshot `json:"sources"`
}

// DeltaResponse replays whole immutable transactions after FromCursor.
type DeltaResponse struct {
	SchemaVersion int           `json:"schema_version"`
	FromCursor    string        `json:"from_cursor"`
	NextCursor    string        `json:"next_cursor"`
	HeadCursor    string        `json:"head_cursor"`
	HasMore       bool          `json:"has_more"`
	Transactions  []Transaction `json:"transactions"`
}

func newGeneration() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Sprintf("generate delta runtime identity: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(value[:])
}

// Cursor returns the current head without consuming journal data.
func (s *Store) Cursor() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cursorFor(s.head)
}

func (s *Store) cursorFor(sequence uint64) string {
	return "v1." + s.generation + "." + strconv.FormatUint(sequence, 10)
}

func parseCursor(value string) (string, uint64, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != "v1" || len(parts[1]) != 22 || parts[2] == "" || (len(parts[2]) > 1 && parts[2][0] == '0') {
		return "", 0, ErrCursorMalformed
	}
	generation, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(generation) != 16 || base64.RawURLEncoding.EncodeToString(generation) != parts[1] {
		return "", 0, ErrCursorMalformed
	}
	for _, character := range parts[2] {
		if character < '0' || character > '9' {
			return "", 0, ErrCursorMalformed
		}
	}
	sequence, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return "", 0, ErrCursorMalformed
	}
	return parts[1], sequence, nil
}

// Full returns sources and cursor under one lock so no poll can fall between them.
func (s *Store) Full() FullResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneLocked(now)
	return FullResponse{SchemaVersion: 1, Generation: s.generation, Cursor: s.cursorFor(s.head), CapturedAt: now, Sources: s.allLocked(now)}
}

// Delta returns detached replay records after cursor without advancing server-side state.
func (s *Store) Delta(cursor string) (DeltaResponse, error) {
	generation, sequence, err := parseCursor(cursor)
	if err != nil {
		return DeltaResponse{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return DeltaResponse{}, ErrCursorGeneration
	}
	if sequence > s.head {
		return DeltaResponse{}, ErrCursorFuture
	}
	s.pruneLocked(s.now().UTC())
	minimum := s.head
	if len(s.journal) > 0 {
		minimum = s.journal[0].Sequence - 1
	}
	if sequence < minimum {
		return DeltaResponse{}, ErrCursorExpired
	}
	end := len(s.journal)
	start := sort.Search(len(s.journal), func(i int) bool { return s.journal[i].Sequence > sequence })
	if end-start > deltaPageSize {
		end = start + deltaPageSize
	}
	transactions := cloneTransactions(s.journal[start:end])
	next := cursor
	if len(transactions) > 0 {
		next = transactions[len(transactions)-1].Cursor
	}
	return DeltaResponse{SchemaVersion: 1, FromCursor: cursor, NextCursor: next, HeadCursor: s.cursorFor(s.head), HasMore: end < len(s.journal), Transactions: transactions}, nil
}

func (s *Store) allLocked(now time.Time) []Snapshot {
	names := make([]string, 0, len(s.m))
	for name := range s.m {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Snapshot, 0, len(names))
	for _, name := range names {
		e := s.m[name]
		value := cloneSnapshot(e.snapshot)
		if !value.LastSuccess.IsZero() && now.Sub(value.LastSuccess) > e.staleAfter {
			value.Stale = true
		}
		out = append(out, value)
	}
	return out
}

func statusFromSnapshot(snapshot Snapshot) SourceStatus {
	return SourceStatus{Name: snapshot.Name, Driver: snapshot.Driver, Available: snapshot.Available, Stale: snapshot.Stale, LastAttempt: snapshot.LastAttempt, LastSuccess: snapshot.LastSuccess, UnavailableSince: snapshot.UnavailableSince, PollDuration: snapshot.PollDuration, Error: snapshot.Error}
}

func newTransaction(source string, status SourceStatus, records []MetricRecord, at time.Time) Transaction {
	if records == nil {
		records = []MetricRecord{}
	}
	return Transaction{CapturedAt: at, Source: source, Status: status, Metrics: records}
}

func (s *Store) recordSuccessLocked(source string, previous []metrics.Metric, snapshot Snapshot, now time.Time) {
	old, oldErr := indexMetrics(previous)
	current, currentErr := indexMetrics(snapshot.Metrics)
	if oldErr != nil || currentErr != nil {
		panic("validated metric identities became invalid")
	}
	emitted := s.lastEmitted[source]
	if emitted == nil {
		emitted = make(map[string]time.Time)
		s.lastEmitted[source] = emitted
	}
	keys := make([]string, 0, len(current)+len(old))
	seen := make(map[string]bool)
	for key := range current {
		keys = append(keys, key)
		seen[key] = true
	}
	for key := range old {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	records := make([]MetricRecord, 0)
	for _, key := range keys {
		currentMetric, exists := current[key]
		oldMetric, existed := old[key]
		if !exists {
			records = append(records, MetricRecord{Operation: "remove", Identity: key})
			delete(emitted, key)
			continue
		}
		changed := !existed || !metricEqualIgnoringTimestamp(oldMetric, currentMetric)
		heartbeat := !changed && (emitted[key].IsZero() || now.Sub(emitted[key]) >= s.options.Heartbeat)
		if changed || heartbeat {
			copy := cloneMetric(currentMetric)
			records = append(records, MetricRecord{Operation: "upsert", Identity: key, Heartbeat: heartbeat, Metric: &copy})
			emitted[key] = now
		}
	}
	s.appendTransactionLocked(newTransaction(source, statusFromSnapshot(snapshot), records, now))
}

func indexMetrics(values []metrics.Metric) (map[string]metrics.Metric, error) {
	out := make(map[string]metrics.Metric, len(values))
	for _, metric := range values {
		identity := metric.Identity
		if identity == "" {
			identity = metric.Name
		}
		if identity == "" {
			return nil, fmt.Errorf("%w: empty identity", ErrInvalidMetrics)
		}
		if _, exists := out[identity]; exists {
			return nil, fmt.Errorf("%w: duplicate identity %q", ErrInvalidMetrics, identity)
		}
		metric.Identity = identity
		out[identity] = metric
	}
	return out, nil
}

func metricEqualIgnoringTimestamp(a, b metrics.Metric) bool {
	a.Timestamp, b.Timestamp = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

func (s *Store) appendTransactionLocked(transaction Transaction) {
	if s.head == math.MaxUint64 {
		s.generation = newGeneration()
		s.head = 0
		s.journal = nil
		s.journalSize = 0
		s.lastEmitted = make(map[string]map[string]time.Time)
	}
	s.head++
	transaction.Sequence = s.head
	transaction.Cursor = s.cursorFor(s.head)
	transaction = cloneTransaction(transaction)
	encoded, err := json.Marshal(transaction)
	if err != nil {
		panic(fmt.Sprintf("marshal delta transaction: %v", err))
	}
	transaction.byteSize = int64(len(encoded))
	s.journal = append(s.journal, transaction)
	s.journalSize += transaction.byteSize
	s.pruneLocked(transaction.CapturedAt)
}

func (s *Store) pruneLocked(now time.Time) {
	for len(s.journal) > 0 && now.Sub(s.journal[0].CapturedAt) > s.options.RetentionAge {
		s.journalSize -= s.journal[0].byteSize
		s.journal = s.journal[1:]
	}
	// Keep the newest transaction when only size/event limits are exceeded so a
	// successful cache update is never silently left without a replay record.
	for len(s.journal) > 1 && (len(s.journal) > s.options.MaxEvents || s.journalSize > s.options.MaxBytes) {
		s.journalSize -= s.journal[0].byteSize
		s.journal = s.journal[1:]
	}
}

func cloneTransactions(in []Transaction) []Transaction {
	out := make([]Transaction, len(in))
	for i := range in {
		out[i] = cloneTransaction(in[i])
	}
	return out
}

func cloneTransaction(in Transaction) Transaction {
	out := in
	out.Discovery = cloneDefinitions(in.Discovery)
	out.Metrics = make([]MetricRecord, len(in.Metrics))
	for i, record := range in.Metrics {
		out.Metrics[i] = record
		if record.Metric != nil {
			metric := cloneMetric(*record.Metric)
			out.Metrics[i].Metric = &metric
		}
	}
	return out
}

func cloneMetric(in metrics.Metric) metrics.Metric {
	out := in
	out.Value = cloneValue(in.Value)
	if in.Labels != nil {
		out.Labels = make(map[string]string, len(in.Labels))
		for key, value := range in.Labels {
			out.Labels[key] = value
		}
	}
	return out
}

func cloneValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneReflect(reflect.ValueOf(value)).Interface()
}

func cloneReflect(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := cloneReflect(value.Elem())
		result := reflect.New(value.Type()).Elem()
		result.Set(copy)
		return result
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.New(value.Type().Elem())
		result.Elem().Set(cloneReflect(value.Elem()))
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(cloneReflect(value.Index(i)))
		}
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			result.SetMapIndex(cloneReflect(iterator.Key()), cloneReflect(iterator.Value()))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(cloneReflect(value.Index(i)))
		}
		return result
	default:
		return value
	}
}
