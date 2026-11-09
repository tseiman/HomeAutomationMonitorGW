package modbusrtu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/config"
	"go.bug.st/serial"
)

type timeoutPort struct {
	mu          sync.Mutex
	readTimeout time.Duration
	timeouts    []time.Duration
}

func (*timeoutPort) SetMode(*serial.Mode) error { return nil }
func (p *timeoutPort) Read([]byte) (int, error) {
	p.mu.Lock()
	timeout := p.readTimeout
	p.mu.Unlock()
	time.Sleep(timeout)
	return 0, io.EOF
}
func (*timeoutPort) Write(data []byte) (int, error) { return len(data), nil }
func (*timeoutPort) Drain() error                   { return nil }
func (*timeoutPort) ResetInputBuffer() error        { return nil }
func (*timeoutPort) ResetOutputBuffer() error       { return nil }
func (*timeoutPort) SetDTR(bool) error              { return nil }
func (*timeoutPort) SetRTS(bool) error              { return nil }
func (*timeoutPort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
}
func (p *timeoutPort) SetReadTimeout(timeout time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readTimeout = timeout
	p.timeouts = append(p.timeouts, timeout)
	return nil
}
func (*timeoutPort) Close() error              { return nil }
func (*timeoutPort) Break(time.Duration) error { return nil }

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

func TestCanonicalBusPathUsesExistingDeviceRdev(t *testing.T) {
	info, err := os.Stat("/dev/null")
	if err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("platform does not expose syscall.Stat_t")
	}
	want := fmt.Sprintf("rdev:%d", uint64(stat.Rdev))
	for _, path := range []string{"/dev/null", "/proc/self/root/dev/null"} {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("existing device alias %q unavailable: %v", path, err)
		}
		if got := canonicalBusPath(path); got != want {
			t.Fatalf("canonicalBusPath(%q)=%q, want %q", path, got, want)
		}
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

func TestSerialSessionBoundsBlockingPortReadByContextDeadline(t *testing.T) {
	port := &timeoutPort{readTimeout: time.Second}
	session := &serialSession{port: port, timeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := session.ReadHoldingRegisters(ctx, 1, 0, 1)
	if err == nil {
		t.Fatal("blocking serial read succeeded")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("serial read ignored context deadline: %v", elapsed)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.timeouts) == 0 || port.timeouts[len(port.timeouts)-1] <= 0 || port.timeouts[len(port.timeouts)-1] > 25*time.Millisecond {
		t.Fatalf("serial read timeouts=%v, want a positive deadline-bounded timeout", port.timeouts)
	}
}

func TestSerialSessionHandlesNoDeadlineAndExpiredDeadline(t *testing.T) {
	port := &timeoutPort{readTimeout: time.Millisecond}
	session := &serialSession{port: port, timeout: time.Millisecond}
	if _, err := session.ReadHoldingRegisters(context.Background(), 1, 0, 1); err == nil {
		t.Fatal("test port unexpectedly returned a valid Modbus response")
	}
	port.mu.Lock()
	if len(port.timeouts) != 1 || port.timeouts[0] != time.Millisecond {
		t.Fatalf("no-deadline timeouts=%v, want configured timeout", port.timeouts)
	}
	port.mu.Unlock()

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := session.ReadHoldingRegisters(expired, 1, 0, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired deadline error=%v", err)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.timeouts) != 1 {
		t.Fatalf("expired request touched port timeouts=%v", port.timeouts)
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
