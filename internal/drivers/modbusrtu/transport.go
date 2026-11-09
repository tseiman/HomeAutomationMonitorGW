package modbusrtu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	modbus "github.com/aldas/go-modbus-client"
	"github.com/aldas/go-modbus-client/packet"
	"github.com/tseiman/HomeAutomationMonitorGW/internal/config"
	"go.bug.st/serial"
)

// ReadTransport is the complete collector-facing Modbus surface. It deliberately exposes only FC01-FC04.
type ReadTransport interface {
	ReadCoils(context.Context, uint8, uint16, uint16) ([]byte, error)
	ReadDiscreteInputs(context.Context, uint8, uint16, uint16) ([]byte, error)
	ReadHoldingRegisters(context.Context, uint8, uint16, uint16) ([]byte, error)
	ReadInputRegisters(context.Context, uint8, uint16, uint16) ([]byte, error)
}

type readSession interface {
	ReadTransport
	SetResponseTimeout(time.Duration) error
	Close() error
}

type serialSettings struct {
	device                    string
	baudRate, dataBits, stops int
	parity                    string
}

type sessionOpener func(serialSettings) (readSession, error)

// BusManager owns one lazily opened transaction-serialized bus for each canonical device path.
type BusManager struct {
	mu     sync.Mutex
	buses  map[string]*sharedBus
	opener sessionOpener
	closed bool
}

// NewBusManager creates a manager backed by go.bug.st/serial and go-modbus-client.
func NewBusManager() *BusManager { return newBusManager(openSerialSession) }

func newBusManager(opener sessionOpener) *BusManager {
	return &BusManager{buses: make(map[string]*sharedBus), opener: opener}
}

// Reader returns a retrying read-only view over the shared physical bus described by source.
func (m *BusManager) Reader(source config.ModbusRTUSource) (ReadTransport, error) {
	identity := canonicalBusPath(source.Device)
	settings := serialSettings{device: source.Device, baudRate: source.BaudRate, dataBits: source.DataBits, stops: source.StopBits, parity: source.Parity}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("Modbus bus manager is closed")
	}
	bus := m.buses[identity]
	if bus == nil {
		bus = &sharedBus{settings: settings, opener: m.opener}
		m.buses[identity] = bus
	} else if !sameSerialFraming(bus.settings, settings) {
		return nil, fmt.Errorf("conflicting serial parameters for physical bus %q", identity)
	}
	return &Reader{bus: bus, timeout: source.ResponseTimeout.Duration, retries: source.Retries}, nil
}

func sameSerialFraming(left, right serialSettings) bool {
	return left.baudRate == right.baudRate && left.dataBits == right.dataBits && left.stops == right.stops && left.parity == right.parity
}

// Close prevents future opens, waits for in-flight transactions, and closes every open port once.
func (m *BusManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	buses := make([]*sharedBus, 0, len(m.buses))
	for _, bus := range m.buses {
		buses = append(buses, bus)
	}
	m.mu.Unlock()
	var joined error
	for _, bus := range buses {
		joined = errors.Join(joined, bus.close())
	}
	return joined
}

type sharedBus struct {
	mu       sync.Mutex
	settings serialSettings
	opener   sessionOpener
	session  readSession
	closed   bool
}

func (b *sharedBus) closeSession() error {
	if b.session == nil {
		return nil
	}
	err := b.session.Close()
	b.session = nil
	return err
}

func (b *sharedBus) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return b.closeSession()
}

func (b *sharedBus) ensureOpen(ctx context.Context, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.closed {
		return errors.New("Modbus bus is closed")
	}
	if b.session == nil {
		session, err := b.opener(b.settings)
		if err != nil {
			return fmt.Errorf("open serial bus %q: %w", b.settings.device, err)
		}
		b.session = session
	}
	if err := b.session.SetResponseTimeout(timeout); err != nil {
		_ = b.closeSession()
		return fmt.Errorf("set serial response timeout: %w", err)
	}
	return nil
}

// Reader applies one source's timeout and additional-attempt count to shared-bus reads.
type Reader struct {
	bus     *sharedBus
	timeout time.Duration
	retries int
}

type readOperation func(context.Context, readSession) ([]byte, error)

func (r *Reader) read(ctx context.Context, operation readOperation) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.bus.mu.Lock()
	defer r.bus.mu.Unlock()
	var last error
	for attempt := 0; attempt <= r.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, r.timeout)
		if err := r.bus.ensureOpen(attemptCtx, r.timeout); err != nil {
			cancel()
			last = err
			continue
		}
		value, err := operation(attemptCtx, r.bus.session)
		cancel()
		if err == nil {
			return value, nil
		}
		last = err
		_ = r.bus.closeSession()
	}
	return nil, fmt.Errorf("Modbus read failed after %d attempt(s): %w", r.retries+1, last)
}

func (r *Reader) ReadCoils(ctx context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	return r.read(ctx, func(ctx context.Context, session readSession) ([]byte, error) {
		return session.ReadCoils(ctx, slave, address, quantity)
	})
}
func (r *Reader) ReadDiscreteInputs(ctx context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	return r.read(ctx, func(ctx context.Context, session readSession) ([]byte, error) {
		return session.ReadDiscreteInputs(ctx, slave, address, quantity)
	})
}
func (r *Reader) ReadHoldingRegisters(ctx context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	return r.read(ctx, func(ctx context.Context, session readSession) ([]byte, error) {
		return session.ReadHoldingRegisters(ctx, slave, address, quantity)
	})
}
func (r *Reader) ReadInputRegisters(ctx context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	return r.read(ctx, func(ctx context.Context, session readSession) ([]byte, error) {
		return session.ReadInputRegisters(ctx, slave, address, quantity)
	})
}

func canonicalBusPath(device string) string {
	cleaned, err := filepath.Abs(filepath.Clean(device))
	if err != nil {
		cleaned = filepath.Clean(device)
	}
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved)
	}
	current := cleaned
	for links := 0; links < 40; links++ {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			break
		}
		target, err := os.Readlink(current)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		current = filepath.Clean(target)
	}
	return current
}

type serialSession struct {
	port    serial.Port
	timeout time.Duration
}

type flushingPort struct{ serial.Port }

func (p flushingPort) Flush() error { return p.ResetInputBuffer() }

func openSerialSession(settings serialSettings) (readSession, error) {
	parity := serial.NoParity
	switch settings.parity {
	case "E":
		parity = serial.EvenParity
	case "O":
		parity = serial.OddParity
	}
	stops := serial.OneStopBit
	if settings.stops == 2 {
		stops = serial.TwoStopBits
	}
	port, err := serial.Open(settings.device, &serial.Mode{BaudRate: settings.baudRate, DataBits: settings.dataBits, Parity: parity, StopBits: stops})
	if err != nil {
		return nil, err
	}
	return &serialSession{port: port, timeout: time.Second}, nil
}

func (s *serialSession) SetResponseTimeout(timeout time.Duration) error {
	s.timeout = timeout
	return s.port.SetReadTimeout(timeout)
}
func (s *serialSession) Close() error { return s.port.Close() }

func (s *serialSession) do(ctx context.Context, request packet.Request) (packet.Response, error) {
	client := modbus.NewSerialClient(flushingPort{s.port}, modbus.WithSerialReadTimeout(s.timeout))
	return client.Do(ctx, request)
}

func (s *serialSession) ReadCoils(ctx context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	req, err := packet.NewReadCoilsRequestRTU(slave, address, quantity)
	if err != nil {
		return nil, err
	}
	response, err := s.do(ctx, req)
	if err != nil {
		return nil, err
	}
	value, ok := response.(*packet.ReadCoilsResponseRTU)
	if !ok || value.UnitID != slave {
		return nil, errors.New("unexpected FC01 response")
	}
	return append([]byte(nil), value.Data...), nil
}
func (s *serialSession) ReadDiscreteInputs(ctx context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	req, err := packet.NewReadDiscreteInputsRequestRTU(slave, address, quantity)
	if err != nil {
		return nil, err
	}
	response, err := s.do(ctx, req)
	if err != nil {
		return nil, err
	}
	value, ok := response.(*packet.ReadDiscreteInputsResponseRTU)
	if !ok || value.UnitID != slave {
		return nil, errors.New("unexpected FC02 response")
	}
	return append([]byte(nil), value.Data...), nil
}
func (s *serialSession) ReadHoldingRegisters(ctx context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	req, err := packet.NewReadHoldingRegistersRequestRTU(slave, address, quantity)
	if err != nil {
		return nil, err
	}
	response, err := s.do(ctx, req)
	if err != nil {
		return nil, err
	}
	value, ok := response.(*packet.ReadHoldingRegistersResponseRTU)
	if !ok || value.UnitID != slave {
		return nil, errors.New("unexpected FC03 response")
	}
	return append([]byte(nil), value.Data...), nil
}
func (s *serialSession) ReadInputRegisters(ctx context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	req, err := packet.NewReadInputRegistersRequestRTU(slave, address, quantity)
	if err != nil {
		return nil, err
	}
	response, err := s.do(ctx, req)
	if err != nil {
		return nil, err
	}
	value, ok := response.(*packet.ReadInputRegistersResponseRTU)
	if !ok || value.UnitID != slave {
		return nil, errors.New("unexpected FC04 response")
	}
	return append([]byte(nil), value.Data...), nil
}
