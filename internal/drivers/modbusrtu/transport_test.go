package modbusrtu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/config"
)

type fakeSession struct {
	mu         sync.Mutex
	calls      []uint8
	active     *int32
	parallel   *int32
	failures   int
	waitForCtx bool
	closed     int32
	response   []byte
}

func (s *fakeSession) read(ctx context.Context, slave uint8) ([]byte, error) {
	if s.active != nil {
		if atomic.AddInt32(s.active, 1) != 1 {
			atomic.StoreInt32(s.parallel, 1)
		}
		defer atomic.AddInt32(s.active, -1)
	}
	s.mu.Lock()
	s.calls = append(s.calls, slave)
	if s.failures > 0 {
		s.failures--
		s.mu.Unlock()
		return nil, errors.New("temporary failure")
	}
	wait := s.waitForCtx
	response := append([]byte(nil), s.response...)
	s.mu.Unlock()
	if wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	time.Sleep(5 * time.Millisecond)
	return response, nil
}
func (s *fakeSession) ReadCoils(ctx context.Context, slave uint8, _, _ uint16) ([]byte, error) {
	return s.read(ctx, slave)
}
func (s *fakeSession) ReadDiscreteInputs(ctx context.Context, slave uint8, _, _ uint16) ([]byte, error) {
	return s.read(ctx, slave)
}
func (s *fakeSession) ReadHoldingRegisters(ctx context.Context, slave uint8, _, _ uint16) ([]byte, error) {
	return s.read(ctx, slave)
}
func (s *fakeSession) ReadInputRegisters(ctx context.Context, slave uint8, _, _ uint16) ([]byte, error) {
	return s.read(ctx, slave)
}
func (s *fakeSession) SetResponseTimeout(time.Duration) error { return nil }
func (s *fakeSession) Close() error                           { atomic.AddInt32(&s.closed, 1); return nil }

func serialConfig(device string, slave uint8) config.ModbusRTUSource {
	return config.ModbusRTUSource{Device: device, BaudRate: 9600, DataBits: 8, Parity: "N", StopBits: 1, ResponseTimeout: config.Duration{Duration: 20 * time.Millisecond}, Retries: 1, SlaveID: slave}
}

func TestBusManagerSharesBusAcrossSlavesAndSerializesTransactions(t *testing.T) {
	var active, parallel int32
	session := &fakeSession{active: &active, parallel: &parallel, response: []byte{0, 1}}
	opens := 0
	manager := newBusManager(func(serialSettings) (readSession, error) { opens++; return session, nil })
	first, err := manager.Reader(serialConfig("/dev/missing-test-bus", 1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Reader(serialConfig("/dev/missing-test-bus", 2))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = first.ReadHoldingRegisters(context.Background(), 1, 0, 1) }()
	go func() { defer wg.Done(); _, _ = second.ReadHoldingRegisters(context.Background(), 2, 0, 1) }()
	wg.Wait()
	if opens != 1 || atomic.LoadInt32(&parallel) != 0 {
		t.Fatalf("opens=%d parallel=%d", opens, parallel)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.calls) != 2 || session.calls[0] == session.calls[1] {
		t.Fatalf("slave calls=%v", session.calls)
	}
}

func TestBusManagerCanonicalizesDanglingDeviceAliases(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "missing-tty")
	alias := filepath.Join(directory, "by-id")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	opens := 0
	manager := newBusManager(func(settings serialSettings) (readSession, error) {
		opens++
		if settings.device != alias {
			t.Fatalf("opened device=%q want stable configured alias %q", settings.device, alias)
		}
		return &fakeSession{response: []byte{0}}, nil
	})
	first, err := manager.Reader(serialConfig(alias, 1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Reader(serialConfig(target, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReadCoils(context.Background(), 1, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ReadCoils(context.Background(), 2, 0, 1); err != nil {
		t.Fatal(err)
	}
	if opens != 1 {
		t.Fatalf("opens=%d", opens)
	}
}

func TestReaderRetriesWithReconnectAndHonorsResponseTimeout(t *testing.T) {
	first := &fakeSession{waitForCtx: true}
	second := &fakeSession{response: []byte{0x12, 0x34}}
	sessions := []*fakeSession{first, second}
	manager := newBusManager(func(serialSettings) (readSession, error) {
		s := sessions[0]
		sessions = sessions[1:]
		return s, nil
	})
	reader, err := manager.Reader(serialConfig("/dev/timeout-test-bus", 7))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	got, err := reader.ReadInputRegisters(context.Background(), 7, 4, 1)
	if err != nil || !reflect.DeepEqual(got, []byte{0x12, 0x34}) {
		t.Fatalf("read=(%v,%v)", got, err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 250*time.Millisecond {
		t.Fatalf("timeout elapsed=%v", elapsed)
	}
	if atomic.LoadInt32(&first.closed) != 1 {
		t.Fatalf("failed session closes=%d", first.closed)
	}
}

func TestBusManagerCloseIsIdempotentAndPreventsReopen(t *testing.T) {
	session := &fakeSession{response: []byte{0}}
	manager := newBusManager(func(serialSettings) (readSession, error) { return session, nil })
	reader, err := manager.Reader(serialConfig("/dev/close-test-bus", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadCoils(context.Background(), 1, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&session.closed) != 1 {
		t.Fatalf("closes=%d", session.closed)
	}
	if _, err := reader.ReadCoils(context.Background(), 1, 0, 1); err == nil {
		t.Fatal("read succeeded after close")
	}
}

func TestCollectorReadAPIHasOnlyFunctionCodesOneThroughFour(t *testing.T) {
	typ := reflect.TypeOf((*ReadTransport)(nil)).Elem()
	var methods []string
	for i := 0; i < typ.NumMethod(); i++ {
		methods = append(methods, typ.Method(i).Name)
	}
	want := []string{"ReadCoils", "ReadDiscreteInputs", "ReadHoldingRegisters", "ReadInputRegisters"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("collector transport methods=%v want %v", methods, want)
	}
}
