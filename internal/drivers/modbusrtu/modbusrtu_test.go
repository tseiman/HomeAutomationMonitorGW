package modbusrtu

import (
	"math"
	"reflect"
	"testing"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/config"
)

func register(name string, address uint16, function uint8, dataType string) config.ModbusRegister {
	return config.ModbusRegister{Name: name, Address: address, Function: function, Type: dataType, Scale: 1}
}

func TestRegisterWidth(t *testing.T) {
	for _, tc := range []struct {
		typeName string
		want     uint16
	}{
		{"bool", 1}, {"uint16", 1}, {"int16", 1}, {"uint32", 2}, {"int32", 2},
		{"float32", 2}, {"uint64", 4}, {"int64", 4}, {"float64", 4},
	} {
		t.Run(tc.typeName, func(t *testing.T) {
			got, err := RegisterWidth(tc.typeName)
			if err != nil || got != tc.want {
				t.Fatalf("RegisterWidth(%q)=(%d, %v), want (%d, nil)", tc.typeName, got, err, tc.want)
			}
		})
	}
	if _, err := RegisterWidth("string"); err == nil {
		t.Fatal("unknown type accepted")
	}
}

func TestDecodeRegistersByteAndWordOrder(t *testing.T) {
	tests := []struct {
		name      string
		dataType  string
		registers []uint16
		wordOrder string
		byteOrder string
		want      float64
	}{
		{"uint16 big", "uint16", []uint16{0x1234}, "high_low", "big", 0x1234},
		{"uint16 little bytes", "uint16", []uint16{0x3412}, "high_low", "little", 0x1234},
		{"int16 negative", "int16", []uint16{0xff9c}, "high_low", "big", -100},
		{"uint32 high low", "uint32", []uint16{0x1234, 0x5678}, "high_low", "big", 0x12345678},
		{"int32 negative low high", "int32", []uint16{0xffff, 0xfffe}, "low_high", "big", -65537},
		{"float32 high low", "float32", []uint16{0x4145, 0x851f}, "high_low", "big", 12.345},
		{"float32 low high", "float32", []uint16{0x851f, 0x4145}, "low_high", "big", 12.345},
		{"uint64", "uint64", []uint16{0x0001, 0x0002, 0x0003, 0x0004}, "high_low", "big", 0x0001000200030004},
		{"int64 negative", "int64", []uint16{0xffff, 0xffff, 0xffff, 0xff9c}, "high_low", "big", -100},
		{"float64", "float64", []uint16{0xc045, 0x2000, 0x0000, 0x0000}, "high_low", "big", -42.25},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			definition := register("value", 0, 3, tc.dataType)
			definition.WordOrder = tc.wordOrder
			definition.ByteOrder = tc.byteOrder
			got, err := DecodeRegisters(definition, tc.registers)
			if err != nil || math.Abs(got-tc.want) > 0.00001 {
				t.Fatalf("DecodeRegisters()=(%v, %v), want %v", got, err, tc.want)
			}
		})
	}
}

func TestDecodeRegistersAppliesScaleAndOffsetAndRejectsNonFinite(t *testing.T) {
	definition := register("current", 74, 4, "float32")
	definition.WordOrder = "low_high"
	definition.ByteOrder = "big"
	definition.Scale = 0.0001
	definition.Offset = -0.5
	got, err := DecodeRegisters(definition, []uint16{0x0000, 0x461c}) // 9984 mA
	if err != nil || math.Abs(got-0.4984) > 0.000001 {
		t.Fatalf("scaled value=(%v, %v)", got, err)
	}

	definition.Scale = 1
	definition.Offset = 0
	if _, err := DecodeRegisters(definition, []uint16{0x0000, 0x7f80}); err == nil {
		t.Fatal("infinity accepted")
	}
	definition.Scale = math.Inf(1)
	if _, err := DecodeRegisters(definition, []uint16{0, 0}); err == nil {
		t.Fatal("non-finite scale accepted")
	}
}

func TestDecodeRegistersSIRAXDCMeasuredVectors(t *testing.T) {
	tests := []struct {
		name            string
		registers       []uint16
		raw             float64
		scale           float64
		want            float64
		rawTolerance    float64
		scaledTolerance float64
	}{
		{name: "voltage", registers: []uint16{0x4dd3, 0x41bc}, raw: 23.538, scale: 1, want: 23.538, rawTolerance: 0.000001, scaledTolerance: 0.000001},
		{name: "current", registers: []uint16{0x7750, 0xc5a1}, raw: -5166.914, scale: -0.0001, want: 0.5166914, rawTolerance: 0.001, scaledTolerance: 0.0000001},
		{name: "active power", registers: []uint16{0x849c, 0xc2f3}, raw: -121.759, scale: -0.1, want: 12.1759, rawTolerance: 0.00001, scaledTolerance: 0.000001},
		{name: "reactive power", registers: []uint16{0x0000, 0x0000}, raw: 0, scale: -0.1, want: 0, rawTolerance: 0, scaledTolerance: 0},
		{name: "apparent power", registers: []uint16{0x3c6a, 0xc2f3}, raw: -121.618, scale: -0.1, want: 12.1618, rawTolerance: 0.00001, scaledTolerance: 0.000001},
		{name: "total energy", registers: []uint16{0x37bd, 0xbe06}, raw: -0.131072, scale: -0.1, want: 0.0131072, rawTolerance: 0.0000001, scaledTolerance: 0.0000001},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			definition := register(tc.name, 0, 3, "float32")
			definition.WordOrder = "low_high"
			definition.ByteOrder = "big"
			definition.Scale = 1
			raw, err := DecodeRegisters(definition, tc.registers)
			if err != nil || math.Abs(raw-tc.raw) > tc.rawTolerance {
				t.Fatalf("raw DecodeRegisters()=(%.10g, %v), want %.10g ± %.1g", raw, err, tc.raw, tc.rawTolerance)
			}
			definition.Scale = tc.scale
			got, err := DecodeRegisters(definition, tc.registers)
			if err != nil || math.Abs(got-tc.want) > tc.scaledTolerance {
				t.Fatalf("scaled DecodeRegisters()=(%.10g, %v), want %.10g ± %.1g", got, err, tc.want, tc.scaledTolerance)
			}
		})
	}
}

func TestDecodeRegistersPreservesGenericNegativeValues(t *testing.T) {
	definition := register("generic-negative", 0, 3, "float32")
	definition.WordOrder = "low_high"
	definition.ByteOrder = "big"
	got, err := DecodeRegisters(definition, []uint16{0x3021, 0xc2e1})
	if err != nil || math.Abs(got-(-112.594)) > 0.00001 {
		t.Fatalf("DecodeRegisters()=(%v, %v), want a preserved negative value", got, err)
	}
}

func TestDecodeRegistersRejects64BitIntegersOutsideExactFloatRange(t *testing.T) {
	words := func(bits uint64) []uint16 {
		return []uint16{uint16(bits >> 48), uint16(bits >> 32), uint16(bits >> 16), uint16(bits)}
	}
	for _, tc := range []struct {
		name     string
		dataType string
		bits     uint64
		want     float64
		wantErr  bool
	}{
		{name: "uint safe boundary", dataType: "uint64", bits: 1 << 53, want: 1 << 53},
		{name: "uint first unsafe", dataType: "uint64", bits: 1<<53 + 1, wantErr: true},
		{name: "uint maximum", dataType: "uint64", bits: math.MaxUint64, wantErr: true},
		{name: "int negative safe boundary", dataType: "int64", bits: ^uint64((1 << 53) - 1), want: -(1 << 53)},
		{name: "int first unsafe negative", dataType: "int64", bits: ^uint64(1 << 53), wantErr: true},
		{name: "int minimum", dataType: "int64", bits: 1 << 63, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := register("counter", 0, 3, tc.dataType)
			definition.WordOrder = "high_low"
			definition.ByteOrder = "big"
			got, err := DecodeRegisters(definition, words(tc.bits))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DecodeRegisters()=%v, want rejection", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("DecodeRegisters()=(%v, %v), want (%v, nil)", got, err, tc.want)
			}
		})
	}
}

func TestDecodeBoolAppliesScaleAndOffsetAndRejectsNonFinite(t *testing.T) {
	definition := register("contact", 9, 1, "bool")
	definition.Scale = 0.5
	definition.Offset = 0.25
	for raw, want := range map[bool]float64{false: 0.25, true: 0.75} {
		got, err := DecodeBool(definition, raw)
		if err != nil || got != want {
			t.Fatalf("DecodeBool(%v)=(%v, %v), want (%v, nil)", raw, got, err, want)
		}
	}
	definition.Scale = math.Inf(1)
	if _, err := DecodeBool(definition, true); err == nil {
		t.Fatal("non-finite bool scaling accepted")
	}
	definition.Scale = math.MaxFloat64
	definition.Offset = math.MaxFloat64
	if _, err := DecodeBool(definition, true); err == nil {
		t.Fatal("non-finite bool result accepted")
	}
	definition.Function = 3
	if _, err := DecodeBool(definition, true); err == nil {
		t.Fatal("bool accepted for register function")
	}
}

func TestDefinitionsAreStaticAndPreserveMetadata(t *testing.T) {
	definitions := Definitions([]config.ModbusRegister{
		{Name: "contact", Address: 1, Function: 1, Type: "bool", Scale: 1},
		{Name: "status", Address: 71, Function: 3, Type: "uint16", Scale: 1},
		{Name: "voltage", Address: 72, Function: 4, Type: "float32", Scale: 1, Unit: "V", Description: "DC voltage"},
	})
	if len(definitions) != 3 || definitions[0].OID != "" || definitions[0].Type != "integer" || definitions[1].Type != "integer" || definitions[2].Name != "voltage" || definitions[2].Type != "number" || definitions[2].Unit != "V" || definitions[2].Description != "DC voltage" {
		t.Fatalf("definitions=%+v", definitions)
	}
}

func TestDefinitionsUseNumberWhenIntegerScalingCanProduceFractions(t *testing.T) {
	definitions := Definitions([]config.ModbusRegister{
		{Name: "fractional_bool", Function: 1, Type: "bool", Scale: 0.5},
		{Name: "fractional_integer", Function: 3, Type: "uint16", Scale: 1, Offset: 0.25},
		{Name: "integral_bool", Function: 2, Type: "bool", Scale: -2, Offset: 1},
		{Name: "integral_integer", Function: 4, Type: "int64", Scale: 3, Offset: -4},
	})
	want := []string{"number", "number", "integer", "integer"}
	for index := range definitions {
		if definitions[index].Type != want[index] {
			t.Fatalf("definition[%d]=%+v, want type %q", index, definitions[index], want[index])
		}
	}
}

func TestPlanReadBlocksBundlesOnlyTouchingCompatibleRanges(t *testing.T) {
	registers := []config.ModbusRegister{
		register("later", 76, 3, "float32"),
		register("voltage", 72, 3, "float32"),
		register("current", 74, 3, "float32"),
		register("gap", 80, 3, "uint16"),
		register("input", 77, 4, "uint16"),
		register("coil", 5, 1, "bool"),
	}
	got, err := PlanReadBlocks(registers, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []ReadBlock{
		{Function: 1, Address: 5, Quantity: 1, MetricNames: []string{"coil"}},
		{Function: 3, Address: 72, Quantity: 6, MetricNames: []string{"voltage", "current", "later"}},
		{Function: 3, Address: 80, Quantity: 1, MetricNames: []string{"gap"}},
		{Function: 4, Address: 77, Quantity: 1, MetricNames: []string{"input"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks=%+v want %+v", got, want)
	}
}

func TestPlanReadBlocksHonorsDisableMaximumAndProtocolLimits(t *testing.T) {
	registers := []config.ModbusRegister{
		register("a", 0, 3, "uint16"), register("b", 1, 3, "uint16"), register("c", 2, 3, "uint16"),
	}
	got, err := PlanReadBlocks(registers, false, 0)
	if err != nil || len(got) != 3 {
		t.Fatalf("disabled blocks=%+v error=%v", got, err)
	}
	got, err = PlanReadBlocks(registers, true, 2)
	if err != nil || len(got) != 2 || got[0].Quantity != 2 || got[1].Address != 2 {
		t.Fatalf("limited blocks=%+v error=%v", got, err)
	}

	many := make([]config.ModbusRegister, 126)
	for i := range many {
		many[i] = register(string(rune('a'+i%26))+string(rune('A'+i/26)), uint16(i), 3, "uint16")
	}
	got, err = PlanReadBlocks(many, true, 0)
	if err != nil || len(got) != 2 || got[0].Quantity != 125 || got[1].Quantity != 1 {
		t.Fatalf("protocol-limited blocks=%+v error=%v", got, err)
	}
}

func TestPlanReadBlocksRejectsOverlapsAndInvalidSpans(t *testing.T) {
	for _, registers := range [][]config.ModbusRegister{
		{register("a", 10, 3, "uint32"), register("b", 11, 3, "uint16")},
		{register("overflow", 65535, 3, "uint32")},
	} {
		if _, err := PlanReadBlocks(registers, true, 0); err == nil {
			t.Fatalf("invalid ranges accepted: %+v", registers)
		}
	}
}
