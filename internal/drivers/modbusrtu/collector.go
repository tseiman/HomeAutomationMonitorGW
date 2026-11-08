package modbusrtu

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/tseiman/HomeAutomationMonitorGW/internal/config"
	"github.com/tseiman/HomeAutomationMonitorGW/internal/metrics"
)

// Collector executes a prevalidated read plan and publishes only complete snapshots.
type Collector struct {
	transport   ReadTransport
	slaveID     uint8
	registers   []config.ModbusRegister
	definitions map[string]config.ModbusRegister
	blocks      []ReadBlock
	Now         func() time.Time
}

// NewCollector plans all reads up front. Polling never discovers or writes device state.
func NewCollector(transport ReadTransport, source config.ModbusRTUSource) (*Collector, error) {
	if transport == nil {
		return nil, errors.New("Modbus read transport is required")
	}
	blocks, err := PlanReadBlocks(source.Registers, source.BundleReads, source.MaxBlockSize)
	if err != nil {
		return nil, err
	}
	definitions := make(map[string]config.ModbusRegister, len(source.Registers))
	for _, definition := range source.Registers {
		definitions[definition.Name] = definition
	}
	return &Collector{transport: transport, slaveID: source.SlaveID, registers: append([]config.ModbusRegister(nil), source.Registers...), definitions: definitions, blocks: blocks}, nil
}

// Discover returns static protocol-neutral metadata without opening the serial device.
func (c *Collector) Discover(context.Context) ([]metrics.Definition, error) {
	return Definitions(c.registers), nil
}

// Poll reads and decodes every planned block into a local snapshot; any error discards all values.
func (c *Collector) Poll(ctx context.Context) ([]metrics.Metric, error) {
	decoded := make(map[string]any, len(c.registers))
	for _, block := range c.blocks {
		data, err := c.readBlock(ctx, block)
		if err != nil {
			return nil, fmt.Errorf("read function %d address %d quantity %d: %w", block.Function, block.Address, block.Quantity, err)
		}
		if err := c.decodeBlock(block, data, decoded); err != nil {
			return nil, err
		}
	}
	at := time.Now().UTC()
	if c.Now != nil {
		at = c.Now().UTC()
	}
	result := make([]metrics.Metric, 0, len(c.registers))
	for _, definition := range c.registers {
		value, ok := decoded[definition.Name]
		if !ok {
			return nil, fmt.Errorf("metric %q was not decoded", definition.Name)
		}
		valueType := "integer"
		if definition.Type == "float32" || definition.Type == "float64" {
			valueType = "number"
		}
		result = append(result, metrics.Metric{Name: definition.Name, Value: value, ValueType: valueType, Unit: definition.Unit, Description: definition.Description, Timestamp: at})
	}
	return result, nil
}

func (c *Collector) readBlock(ctx context.Context, block ReadBlock) ([]byte, error) {
	switch block.Function {
	case 1:
		return c.transport.ReadCoils(ctx, c.slaveID, block.Address, block.Quantity)
	case 2:
		return c.transport.ReadDiscreteInputs(ctx, c.slaveID, block.Address, block.Quantity)
	case 3:
		return c.transport.ReadHoldingRegisters(ctx, c.slaveID, block.Address, block.Quantity)
	case 4:
		return c.transport.ReadInputRegisters(ctx, c.slaveID, block.Address, block.Quantity)
	default:
		return nil, fmt.Errorf("unsupported read function %d", block.Function)
	}
}

func (c *Collector) decodeBlock(block ReadBlock, data []byte, decoded map[string]any) error {
	if block.Function == 1 || block.Function == 2 {
		expected := int(block.Quantity+7) / 8
		if len(data) != expected {
			return fmt.Errorf("function %d address %d returned %d data bytes, want %d", block.Function, block.Address, len(data), expected)
		}
		for _, name := range block.MetricNames {
			definition := c.definitions[name]
			bit := uint32(definition.Address) - uint32(block.Address)
			if bit >= uint32(block.Quantity) {
				return fmt.Errorf("metric %q lies outside its read block", name)
			}
			set := data[bit/8]&(1<<uint(bit%8)) != 0
			value, err := DecodeBool(definition, set)
			if err != nil {
				return err
			}
			if value {
				decoded[name] = int64(1)
			} else {
				decoded[name] = int64(0)
			}
		}
		return nil
	}

	expected := int(block.Quantity) * 2
	if len(data) != expected {
		return fmt.Errorf("function %d address %d returned %d data bytes, want %d", block.Function, block.Address, len(data), expected)
	}
	registers := make([]uint16, block.Quantity)
	for index := range registers {
		registers[index] = binary.BigEndian.Uint16(data[index*2:])
	}
	for _, name := range block.MetricNames {
		definition := c.definitions[name]
		width, err := RegisterWidth(definition.Type)
		if err != nil {
			return err
		}
		start := uint32(definition.Address) - uint32(block.Address)
		end := start + uint32(width)
		if end > uint32(len(registers)) {
			return fmt.Errorf("metric %q lies outside its read block", name)
		}
		value, err := DecodeRegisters(definition, registers[start:end])
		if err != nil {
			return err
		}
		decoded[name] = value
	}
	return nil
}
