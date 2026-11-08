package modbusrtu

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/config"
)

type fakeReadTransport struct {
	responses map[readKey][]byte
	errors    map[readKey]error
	calls     []readKey
}
type readKey struct {
	function          uint8
	slave             uint8
	address, quantity uint16
}

func (f *fakeReadTransport) read(key readKey) ([]byte, error) {
	f.calls = append(f.calls, key)
	if err := f.errors[key]; err != nil {
		return nil, err
	}
	return append([]byte(nil), f.responses[key]...), nil
}
func (f *fakeReadTransport) ReadCoils(_ context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	return f.read(readKey{1, slave, address, quantity})
}
func (f *fakeReadTransport) ReadDiscreteInputs(_ context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	return f.read(readKey{2, slave, address, quantity})
}
func (f *fakeReadTransport) ReadHoldingRegisters(_ context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	return f.read(readKey{3, slave, address, quantity})
}
func (f *fakeReadTransport) ReadInputRegisters(_ context.Context, slave uint8, address, quantity uint16) ([]byte, error) {
	return f.read(readKey{4, slave, address, quantity})
}

func collectorSource(registers ...config.ModbusRegister) config.ModbusRTUSource {
	return config.ModbusRTUSource{SlaveID: 9, BundleReads: true, Registers: registers}
}

func TestCollectorExecutesPlannedBlocksDecodesBitsAndAllRegisterValues(t *testing.T) {
	coilA := register("coil_a", 10, 1, "bool")
	coilB := register("coil_b", 11, 1, "bool")
	status := register("status", 20, 3, "uint16")
	status.WordOrder = "high_low"
	status.ByteOrder = "big"
	temperature := register("temperature", 21, 3, "int16")
	temperature.WordOrder = "high_low"
	temperature.ByteOrder = "big"
	voltage := register("voltage", 22, 3, "float32")
	voltage.WordOrder = "high_low"
	voltage.ByteOrder = "big"
	voltage.Unit = "V"
	transport := &fakeReadTransport{responses: map[readKey][]byte{
		{1, 9, 10, 2}: {0b00000001},
		{3, 9, 20, 4}: {0x00, 0x2a, 0xff, 0x9c, 0x41, 0x48, 0x00, 0x00},
	}, errors: map[readKey]error{}}
	collector, err := NewCollector(transport, collectorSource(coilA, coilB, status, temperature, voltage))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	collector.Now = func() time.Time { return at }
	got, err := collector.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("metrics=%+v", got)
	}
	wantValues := []any{int64(1), int64(0), float64(42), float64(-100), float64(12.5)}
	wantTypes := []string{"integer", "integer", "integer", "integer", "number"}
	for i := range got {
		if got[i].Name != []string{"coil_a", "coil_b", "status", "temperature", "voltage"}[i] || !reflect.DeepEqual(got[i].Value, wantValues[i]) || got[i].ValueType != wantTypes[i] || got[i].Timestamp != at {
			t.Fatalf("metric[%d]=%+v want value=%#v type=%s", i, got[i], wantValues[i], wantTypes[i])
		}
	}
	if got[4].Unit != "V" || len(transport.calls) != 2 {
		t.Fatalf("metrics=%+v calls=%v", got, transport.calls)
	}
}

func TestCollectorRejectsWrongResponseLength(t *testing.T) {
	definition := register("value", 4, 4, "uint16")
	definition.WordOrder = "high_low"
	definition.ByteOrder = "big"
	key := readKey{4, 9, 4, 1}
	for _, response := range [][]byte{{0x12}, {0x12, 0x34, 0x56}} {
		transport := &fakeReadTransport{responses: map[readKey][]byte{key: response}, errors: map[readKey]error{}}
		collector, err := NewCollector(transport, collectorSource(definition))
		if err != nil {
			t.Fatal(err)
		}
		if values, err := collector.Poll(context.Background()); err == nil || values != nil {
			t.Fatalf("response %x yielded values=%v error=%v", response, values, err)
		}
	}
}

func TestCollectorReturnsNoPartialSnapshotOnBlockFailureOrNonfiniteDecode(t *testing.T) {
	first := register("first", 1, 3, "uint16")
	first.WordOrder = "high_low"
	first.ByteOrder = "big"
	second := register("second", 9, 4, "float32")
	second.WordOrder = "high_low"
	second.ByteOrder = "big"
	firstKey, secondKey := readKey{3, 9, 1, 1}, readKey{4, 9, 9, 2}
	cases := []struct {
		name      string
		responses map[readKey][]byte
		errors    map[readKey]error
	}{
		{"block failure", map[readKey][]byte{firstKey: {0, 7}}, map[readKey]error{secondKey: errors.New("device exception")}},
		{"nonfinite", map[readKey][]byte{firstKey: {0, 7}, secondKey: {0x7f, 0x80, 0, 0}}, map[readKey]error{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			collector, err := NewCollector(&fakeReadTransport{responses: tc.responses, errors: tc.errors}, collectorSource(first, second))
			if err != nil {
				t.Fatal(err)
			}
			values, err := collector.Poll(context.Background())
			if err == nil || values != nil {
				t.Fatalf("values=%v error=%v", values, err)
			}
		})
	}
}

func TestCollectorRejectsNonfiniteScaledInteger(t *testing.T) {
	definition := register("overflow", 1, 3, "uint64")
	definition.WordOrder = "high_low"
	definition.ByteOrder = "big"
	definition.Scale = math.MaxFloat64
	key := readKey{3, 9, 1, 4}
	collector, err := NewCollector(&fakeReadTransport{responses: map[readKey][]byte{key: {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}, errors: map[readKey]error{}}, collectorSource(definition))
	if err != nil {
		t.Fatal(err)
	}
	if values, err := collector.Poll(context.Background()); err == nil || values != nil {
		t.Fatalf("values=%v error=%v", values, err)
	}
}
