package main

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// textEncoder 仅用于 --debug：所有字段渲染为稳定的 key=value，避免和 JSON 混排。
type textEncoder struct {
	fields  *zapcore.MapObjectEncoder
	colored bool
	pool    buffer.Pool
}

func newTextEncoder(colored bool) zapcore.Encoder {
	return &textEncoder{fields: zapcore.NewMapObjectEncoder(), colored: colored, pool: buffer.NewPool()}
}

func (e *textEncoder) Clone() zapcore.Encoder {
	fields := zapcore.NewMapObjectEncoder()
	for key, value := range e.fields.Fields {
		fields.Fields[key] = value
	}
	return &textEncoder{fields: fields, colored: e.colored, pool: e.pool}
}

func (e *textEncoder) EncodeEntry(entry zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	clone := e.Clone().(*textEncoder)
	for _, field := range fields {
		field.AddTo(clone.fields)
	}
	buf := e.pool.Get()
	buf.AppendString(entry.Time.Format("2006-01-02 15:04:05.000 -07:00"))
	buf.AppendByte('\t')
	buf.AppendString(e.level(entry.Level))
	buf.AppendByte('\t')
	buf.AppendString(entry.Message)
	keys := make([]string, 0, len(clone.fields.Fields))
	for key := range clone.fields.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		buf.AppendByte(' ')
		buf.AppendString(key)
		buf.AppendByte('=')
		buf.AppendString(formatLogValue(clone.fields.Fields[key]))
	}
	buf.AppendByte('\n')
	return buf, nil
}

func (e *textEncoder) level(level zapcore.Level) string {
	text := level.CapitalString()
	if !e.colored {
		return text
	}
	color := map[zapcore.Level]string{
		zapcore.DebugLevel:  "36",
		zapcore.InfoLevel:   "32",
		zapcore.WarnLevel:   "33",
		zapcore.ErrorLevel:  "31",
		zapcore.DPanicLevel: "31",
		zapcore.PanicLevel:  "31",
		zapcore.FatalLevel:  "31",
	}[level]
	if color == "" {
		return text
	}
	return "\x1b[" + color + "m" + text + "\x1b[0m"
}

func formatLogValue(value any) string {
	switch value := value.(type) {
	case string:
		return strconv.Quote(value)
	case error:
		return strconv.Quote(value.Error())
	default:
		return fmt.Sprint(value)
	}
}

func (e *textEncoder) AddArray(key string, value zapcore.ArrayMarshaler) error {
	return e.fields.AddArray(key, value)
}
func (e *textEncoder) AddObject(key string, value zapcore.ObjectMarshaler) error {
	return e.fields.AddObject(key, value)
}
func (e *textEncoder) AddBinary(key string, value []byte)          { e.fields.AddBinary(key, value) }
func (e *textEncoder) AddByteString(key string, value []byte)      { e.fields.AddByteString(key, value) }
func (e *textEncoder) AddBool(key string, value bool)              { e.fields.AddBool(key, value) }
func (e *textEncoder) AddComplex128(key string, value complex128)  { e.fields.AddComplex128(key, value) }
func (e *textEncoder) AddComplex64(key string, value complex64)    { e.fields.AddComplex64(key, value) }
func (e *textEncoder) AddDuration(key string, value time.Duration) { e.fields.AddDuration(key, value) }
func (e *textEncoder) AddFloat64(key string, value float64)        { e.fields.AddFloat64(key, value) }
func (e *textEncoder) AddFloat32(key string, value float32)        { e.fields.AddFloat32(key, value) }
func (e *textEncoder) AddInt(key string, value int)                { e.fields.AddInt(key, value) }
func (e *textEncoder) AddInt64(key string, value int64)            { e.fields.AddInt64(key, value) }
func (e *textEncoder) AddInt32(key string, value int32)            { e.fields.AddInt32(key, value) }
func (e *textEncoder) AddInt16(key string, value int16)            { e.fields.AddInt16(key, value) }
func (e *textEncoder) AddInt8(key string, value int8)              { e.fields.AddInt8(key, value) }
func (e *textEncoder) AddString(key, value string)                 { e.fields.AddString(key, value) }
func (e *textEncoder) AddTime(key string, value time.Time)         { e.fields.AddTime(key, value) }
func (e *textEncoder) AddUint(key string, value uint)              { e.fields.AddUint(key, value) }
func (e *textEncoder) AddUint64(key string, value uint64)          { e.fields.AddUint64(key, value) }
func (e *textEncoder) AddUint32(key string, value uint32)          { e.fields.AddUint32(key, value) }
func (e *textEncoder) AddUint16(key string, value uint16)          { e.fields.AddUint16(key, value) }
func (e *textEncoder) AddUint8(key string, value uint8)            { e.fields.AddUint8(key, value) }
func (e *textEncoder) AddUintptr(key string, value uintptr)        { e.fields.AddUintptr(key, value) }
func (e *textEncoder) AddReflected(key string, value any) error {
	return e.fields.AddReflected(key, value)
}
func (e *textEncoder) OpenNamespace(key string) { e.fields.OpenNamespace(key) }

var _ zapcore.Encoder = (*textEncoder)(nil)
