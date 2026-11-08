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

func TestDecodeBoolOnlyAcceptsBitFunctions(t *testing.T) {
	definition := register("contact", 9, 1, "bool")
	got, err := DecodeBool(definition, true)
	if err != nil || !got {
		t.Fatalf("DecodeBool()=(%v, %v)", got, err)
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
