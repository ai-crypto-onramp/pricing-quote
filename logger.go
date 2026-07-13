package main

import (
	"log"
	"os"
	"strings"
	"sync"
)

// logger is a minimal structured (JSON-ish) leveled logger. It keeps deps to
// the std library and is safe for concurrent use. Levels: debug, info, warn,
// error. Below the configured level, calls are no-ops.
type logger struct {
	mu    sync.Mutex
	level int
	std   *log.Logger
}

const (
	lvlDebug = iota
	lvlInfo
	lvlWarn
	lvlError
)

// newLogger builds a logger from the configured LOG_LEVEL (default info).
func newLogger(level string) *logger {
	l := &logger{std: log.New(os.Stdout, "", log.LstdFlags|log.LUTC)}
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		l.level = lvlDebug
	case "warn":
		l.level = lvlWarn
	case "error":
		l.level = lvlError
	default:
		l.level = lvlInfo
	}
	return l
}

// pkgLogger is a default logger used by package-level log helpers (logWarn,
// logInfo) when no Server logger is configured. It defaults to info level.
var pkgLogger = newLogger("info")

func setPkgLogger(l *logger) { if l != nil { pkgLogger = l } }

func logWarn(msg string, fields ...field) { pkgLogger.Warn(msg, fields...) }
func logInfo(msg string, fields ...field)  { pkgLogger.Info(msg, fields...) }

func (l *logger) Debug(msg string, fields ...field) { l.log(lvlDebug, msg, fields) }
func (l *logger) Info(msg string, fields ...field)  { l.log(lvlInfo, msg, fields) }
func (l *logger) Warn(msg string, fields ...field)  { l.log(lvlWarn, msg, fields) }
func (l *logger) Error(msg string, fields ...field) { l.log(lvlError, msg, fields) }

func (l *logger) log(level int, msg string, fields []field) {
	if level < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	b.WriteString(`{"msg":"`)
	b.WriteString(jsonEscape(msg))
	b.WriteByte('"')
	for _, f := range fields {
		b.WriteString(`,"`)
		b.WriteString(jsonEscape(f.k))
		b.WriteString(`":`)
		b.WriteString(f.json())
	}
	b.WriteString("}")
	_ = l.std.Output(2, b.String())
}

// field is a structured log field.
type field struct {
	k string
	v  any
}

func fStr(k, v string) field    { return field{k: k, v: v} }
func fInt(k string, v int) field  { return field{k: k, v: v} }
func fErr(err error) field       { return field{k: "err", v: err.Error()} }

func (f field) json() string {
	switch v := f.v.(type) {
	case string:
		return `"` + jsonEscape(v) + `"`
	case int:
		return itoa(v)
	case error:
		return `"` + jsonEscape(v.Error()) + `"`
	default:
		return `"` + jsonEscape(formatAny(v)) + `"`
	}
}

func jsonEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func formatAny(v any) string {
	return "" // placeholder; extend as needed
}