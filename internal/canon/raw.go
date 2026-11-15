// Package canon turns values coming from different database engines into one
// engine independent canonical representation.  Comparison, chunk checksums
// and repair SQL generation all operate on this representation, so a value
// stored as GBK text in the legacy database and UTF-8 text in the new one (or
// as 123.4500 vs 123.45) compares equal.
package canon

import (
	"fmt"
	"time"
)

// Raw is one value as it came out of a database driver.
type Raw struct {
	Null bool
	// Kind is 's' (string), 'b' (bytes), 'i' (integer), 'f' (float),
	// 't' (time.Time) or 0 (unknown, treated as string).
	Kind byte
	S    string
	B    []byte
	I    int64
	F    float64
	T    time.Time
}

// Str builds a Raw from a string.
func Str(s string) Raw { return Raw{Kind: 's', S: s} }

// Bytes builds a Raw from a byte slice.
func Bytes(b []byte) Raw { return Raw{Kind: 'b', B: b} }

// Int builds a Raw from an int64.
func Int(i int64) Raw { return Raw{Kind: 'i', I: i} }

// Float builds a Raw from a float64.
func Float(f float64) Raw { return Raw{Kind: 'f', F: f} }

// Time builds a Raw from a time.Time.
func Time(t time.Time) Raw { return Raw{Kind: 't', T: t} }

// NullVal builds a NULL Raw.
func NullVal() Raw { return Raw{Null: true} }

// FromDriver converts a driver value into a Raw.
func FromDriver(v any) Raw {
	switch x := v.(type) {
	case nil:
		return NullVal()
	case string:
		return Str(x)
	case []byte:
		return Bytes(x)
	case int64:
		return Int(x)
	case int32:
		return Int(int64(x))
	case int:
		return Int(int64(x))
	case float64:
		return Float(x)
	case bool:
		if x {
			return Int(1)
		}
		return Int(0)
	case time.Time:
		return Time(x)
	default:
		return Str(fmt.Sprint(x))
	}
}
