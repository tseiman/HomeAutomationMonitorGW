// Package modbusrtu provides pure Modbus RTU configuration helpers.
// It intentionally contains no serial transport or device I/O.
package modbusrtu

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/config"
	"github.com/tseiman/HomeAutomationMonitorGW/internal/metrics"
)

// ReadBlock is one safe contiguous read request. Addresses are zero-based PDU offsets.
type ReadBlock struct {
	Function    uint8
	Address     uint16
	Quantity    uint16
	MetricNames []string
}

// RegisterWidth returns the number of bits (bool) or 16-bit registers occupied by a value.
func RegisterWidth(dataType string) (uint16, error) {
	switch dataType {
	case "bool", "uint16", "int16":
		return 1, nil
	case "uint32", "int32", "float32":
		return 2, nil
	case "uint64", "int64", "float64":
		return 4, nil
	default:
		return 0, fmt.Errorf("unsupported Modbus type %q", dataType)
	}
}

// DecodeBool decodes a bit as raw 0/1, then applies physical = raw*scale+offset.
func DecodeBool(definition config.ModbusRegister, value bool) (float64, error) {
	if definition.Type != "bool" {
		return 0, fmt.Errorf("metric %q is not bool", definition.Name)
	}
	if definition.Function != 1 && definition.Function != 2 {
		return 0, fmt.Errorf("bool metric %q requires function 1 or 2", definition.Name)
	}
	if !finite(definition.Scale) || !finite(definition.Offset) {
		return 0, fmt.Errorf("metric %q contains a non-finite value", definition.Name)
	}
	raw := float64(0)
	if value {
		raw = 1
	}
	physical := raw*definition.Scale + definition.Offset
	if !finite(physical) {
		return 0, fmt.Errorf("metric %q scaling produced a non-finite value", definition.Name)
	}
	return physical, nil
}

// DecodeRegisters decodes a numeric value, then applies physical = raw*scale+offset.
func DecodeRegisters(definition config.ModbusRegister, registers []uint16) (float64, error) {
	if definition.Function != 3 && definition.Function != 4 {
		return 0, fmt.Errorf("numeric metric %q requires function 3 or 4", definition.Name)
	}
	width, err := RegisterWidth(definition.Type)
	if err != nil {
		return 0, err
	}
	if definition.Type == "bool" {
		return 0, fmt.Errorf("bool metric %q cannot be decoded from registers", definition.Name)
	}
	if len(registers) != int(width) {
		return 0, fmt.Errorf("metric %q requires %d registers, got %d", definition.Name, width, len(registers))
	}
	if definition.WordOrder != "high_low" && definition.WordOrder != "low_high" {
		return 0, fmt.Errorf("metric %q has invalid word order %q", definition.Name, definition.WordOrder)
	}
	if definition.ByteOrder != "big" && definition.ByteOrder != "little" {
		return 0, fmt.Errorf("metric %q has invalid byte order %q", definition.Name, definition.ByteOrder)
	}

	words := append([]uint16(nil), registers...)
	if definition.WordOrder == "low_high" {
		for left, right := 0, len(words)-1; left < right; left, right = left+1, right-1 {
			words[left], words[right] = words[right], words[left]
		}
	}
	data := make([]byte, len(words)*2)
	for index, word := range words {
		if definition.ByteOrder == "little" {
			word = word<<8 | word>>8
		}
		binary.BigEndian.PutUint16(data[index*2:], word)
	}

	var raw float64
	switch definition.Type {
	case "uint16":
		raw = float64(binary.BigEndian.Uint16(data))
	case "int16":
		raw = float64(int16(binary.BigEndian.Uint16(data)))
	case "uint32":
		raw = float64(binary.BigEndian.Uint32(data))
	case "int32":
		raw = float64(int32(binary.BigEndian.Uint32(data)))
	case "float32":
		raw = float64(math.Float32frombits(binary.BigEndian.Uint32(data)))
	case "uint64":
		value := binary.BigEndian.Uint64(data)
		if value > 1<<53 {
			return 0, fmt.Errorf("metric %q uint64 value exceeds exact JSON integer range", definition.Name)
		}
		raw = float64(value)
	case "int64":
		value := int64(binary.BigEndian.Uint64(data))
		if value < -(1<<53) || value > 1<<53 {
			return 0, fmt.Errorf("metric %q int64 value exceeds exact JSON integer range", definition.Name)
		}
		raw = float64(value)
	case "float64":
		raw = math.Float64frombits(binary.BigEndian.Uint64(data))
	default:
		return 0, fmt.Errorf("unsupported Modbus type %q", definition.Type)
	}
	if !finite(raw) || !finite(definition.Scale) || !finite(definition.Offset) {
		return 0, fmt.Errorf("metric %q contains a non-finite value", definition.Name)
	}
	physical := raw*definition.Scale + definition.Offset
	if !finite(physical) {
		return 0, fmt.Errorf("metric %q scaling produced a non-finite value", definition.Name)
	}
	return physical, nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// Definitions returns discovery metadata without contacting a device.
func Definitions(registers []config.ModbusRegister) []metrics.Definition {
	definitions := make([]metrics.Definition, len(registers))
	for index, register := range registers {
		definitions[index] = metrics.Definition{
			Name: register.Name, Type: metricValueType(register), Unit: register.Unit, Description: register.Description,
		}
	}
	return definitions
}

func metricValueType(register config.ModbusRegister) string {
	if register.Type == "float32" || register.Type == "float64" || !finite(register.Scale) || !finite(register.Offset) || math.Trunc(register.Scale) != register.Scale || math.Trunc(register.Offset) != register.Offset {
		return "number"
	}
	return "integer"
}

type plannedMetric struct {
	definition config.ModbusRegister
	width      uint16
	end        uint32
}

// PlanReadBlocks groups only touching ranges with equal function codes.
// A zero maximum uses the Modbus protocol limit (2000 bits or 125 registers).
func PlanReadBlocks(registers []config.ModbusRegister, bundle bool, maximum uint16) ([]ReadBlock, error) {
	planned := make([]plannedMetric, 0, len(registers))
	for _, definition := range registers {
		width, err := RegisterWidth(definition.Type)
		if err != nil {
			return nil, err
		}
		if definition.Type == "bool" {
			if definition.Function != 1 && definition.Function != 2 {
				return nil, fmt.Errorf("bool metric %q requires function 1 or 2", definition.Name)
			}
		} else if definition.Function != 3 && definition.Function != 4 {
			return nil, fmt.Errorf("numeric metric %q requires function 3 or 4", definition.Name)
		}
		end := uint32(definition.Address) + uint32(width)
		if end > 65536 {
			return nil, fmt.Errorf("metric %q has an invalid address span", definition.Name)
		}
		planned = append(planned, plannedMetric{definition, width, end})
	}
	sort.SliceStable(planned, func(i, j int) bool {
		if planned[i].definition.Function != planned[j].definition.Function {
			return planned[i].definition.Function < planned[j].definition.Function
		}
		return planned[i].definition.Address < planned[j].definition.Address
	})

	blocks := make([]ReadBlock, 0, len(planned))
	for _, metric := range planned {
		limit := protocolLimit(metric.definition.Function)
		if maximum > 0 && maximum < limit {
			limit = maximum
		}
		if metric.width > limit {
			return nil, fmt.Errorf("metric %q exceeds maximum block size", metric.definition.Name)
		}
		if len(blocks) > 0 {
			previous := &blocks[len(blocks)-1]
			previousEnd := uint32(previous.Address) + uint32(previous.Quantity)
			start := uint32(metric.definition.Address)
			if previous.Function == metric.definition.Function && start < previousEnd {
				return nil, fmt.Errorf("metrics overlap at address %d for function %d", start, previous.Function)
			}
			combined := metric.end - uint32(previous.Address)
			if bundle && previous.Function == metric.definition.Function && start == previousEnd && combined <= uint32(limit) {
				previous.Quantity = uint16(combined)
				previous.MetricNames = append(previous.MetricNames, metric.definition.Name)
				continue
			}
		}
		blocks = append(blocks, ReadBlock{Function: metric.definition.Function, Address: metric.definition.Address, Quantity: metric.width, MetricNames: []string{metric.definition.Name}})
	}
	return blocks, nil
}

func protocolLimit(function uint8) uint16 {
	if function == 1 || function == 2 {
		return 2000
	}
	return 125
}
