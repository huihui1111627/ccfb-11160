package plan

import (
	"fmt"
	"math"
	"time"

	"github.com/shopspring/decimal"

	"bankcmp/internal/model"
)

// KeyVal is one typed key component, comparable in Go exactly the way the
// database orders it.
type KeyVal struct {
	Type  model.LogicalType
	Null  bool
	Int   int64
	Dec   decimal.Decimal
	Str   string
	Time  time.Time
	Bytes []byte
}

// Compare returns -1, 0 or 1 comparing a and b.  Types must agree.
func Compare(a, b KeyVal) int {
	if a.Null || b.Null {
		switch {
		case a.Null && b.Null:
			return 0
		case a.Null:
			return -1
		default:
			return 1
		}
	}
	switch a.Type {
	case model.TypeInt:
		return cmpInt(a.Int, b.Int)
	case model.TypeDecimal, model.TypeFloat:
		return a.Dec.Cmp(b.Dec)
	case model.TypeDate, model.TypeDateTime, model.TypeTime:
		return cmpInt(a.Time.UnixNano(), b.Time.UnixNano())
	case model.TypeBytes:
		return cmpBytes(a.Bytes, b.Bytes)
	default:
		return cmpString(a.Str, b.Str)
	}
}

// CompareTuple compares composite keys lexicographically.
func CompareTuple(a, b []KeyVal) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if c := Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmpInt(int64(len(a)), int64(len(b)))
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpBytes(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return cmpInt(int64(len(a)), int64(len(b)))
}

// FormatKeyVal renders a key value for display and for the diff record.
func FormatKeyVal(k KeyVal) (string, bool) {
	if k.Null {
		return "", true
	}
	switch k.Type {
	case model.TypeInt:
		return fmt.Sprintf("%d", k.Int), false
	case model.TypeDecimal:
		return k.Dec.String(), false
	case model.TypeFloat:
		return model.FormatFloat(k.Dec.InexactFloat64()), false
	case model.TypeDate:
		return k.Time.UTC().Format("2006-01-02"), false
	case model.TypeTime:
		return k.Time.UTC().Format("15:04:05.999999999"), false
	case model.TypeDateTime:
		return k.Time.UTC().Format(time.RFC3339Nano), false
	case model.TypeBytes:
		return fmt.Sprintf("%x", k.Bytes), false
	default:
		return k.Str, false
	}
}

// Midpoint returns a value strictly between a and b for a single key
// component, or ok=false when none can be constructed.  The caller only uses
// it to split a range, so an uneven split is acceptable but lo < mid < hi must
// hold.
func Midpoint(a, b KeyVal) (KeyVal, bool) {
	switch a.Type {
	case model.TypeInt:
		if b.Int-a.Int < 2 {
			return KeyVal{}, false
		}
		return KeyVal{Type: a.Type, Int: a.Int + (b.Int-a.Int)/2}, true
	case model.TypeDecimal, model.TypeFloat:
		mid := a.Dec.Add(b.Dec).Div(decimal.NewFromInt(2))
		if mid.Cmp(a.Dec) <= 0 || mid.Cmp(b.Dec) >= 0 {
			return KeyVal{}, false
		}
		return KeyVal{Type: a.Type, Dec: mid}, true
	case model.TypeDate, model.TypeDateTime, model.TypeTime:
		lo, hi := a.Time.UnixNano(), b.Time.UnixNano()
		if hi-lo < 2 {
			return KeyVal{}, false
		}
		mid := time.Unix(0, lo+(hi-lo)/2).UTC()
		if !mid.After(a.Time) || !mid.Before(b.Time) {
			return KeyVal{}, false
		}
		return KeyVal{Type: a.Type, Time: mid}, true
	case model.TypeBytes:
		mid, ok := midpointBytes(a.Bytes, b.Bytes)
		if !ok {
			return KeyVal{}, false
		}
		return KeyVal{Type: a.Type, Bytes: mid}, true
	default:
		mid, ok := midpointString(a.Str, b.Str)
		if !ok {
			return KeyVal{}, false
		}
		return KeyVal{Type: a.Type, Str: mid}, true
	}
}

// midpointString finds a string strictly between a and b using the same
// bytewise ordering the databases use (COLLATE C / BINARY).
func midpointString(a, b string) (string, bool) {
	if a >= b {
		return "", false
	}
	ab, bb := []byte(a), []byte(b)
	// common prefix
	i := 0
	for i < len(ab) && i < len(bb) && ab[i] == bb[i] {
		i++
	}
	// Try to find a position i where a has a byte with room to increment and
	// the result is still < b.
	if i < len(ab) && ab[i] < 0xFF {
		// increment a[i]
		res := append([]byte(nil), ab[:i+1]...)
		res[i]++
		// strip trailing zero bytes to keep the result tight
		for len(res) > 0 && res[len(res)-1] == 0 {
			res = res[:len(res)-1]
		}
		s := string(res)
		if s > a && s < b {
			return s, true
		}
	}
	// Fall back to appending a NUL-free separator to a.
	if i < len(ab) {
		return "", false
	}
	// a is a prefix of b: append a byte that keeps it below b.
	if len(bb) > len(ab) && bb[len(ab)] > 0x01 {
		// pick a byte below b's next byte and >= 0x01
		c := byte(0x01)
		res := append(append([]byte(nil), ab...), c)
		s := string(res)
		if s > a && s < b {
			return s, true
		}
	}
	return "", false
}

func midpointBytes(a, b []byte) ([]byte, bool) {
	s, ok := midpointString(string(a), string(b))
	if !ok {
		return nil, false
	}
	return []byte(s), true
}

// KeyArg renders a KeyVal as a database argument with the right Go type.
func KeyArg(k KeyVal) any {
	if k.Null {
		return nil
	}
	switch k.Type {
	case model.TypeInt:
		return k.Int
	case model.TypeDecimal, model.TypeFloat:
		// Send numbers as text so the driver never rounds through float; the
		// SQL placeholder is cast to numeric.
		return k.Dec.String()
	case model.TypeDate:
		return k.Time.Format("2006-01-02")
	case model.TypeTime:
		return k.Time.Format("15:04:05.999999999")
	case model.TypeDateTime:
		return k.Time.UTC()
	case model.TypeBytes:
		return k.Bytes
	default:
		return k.Str
	}
}

// Successor returns the smallest key value strictly greater than k for types
// where one can be represented.  It is used to split half open range bounds.
func Successor(k KeyVal) (KeyVal, bool) {
	switch k.Type {
	case model.TypeInt:
		if k.Int == math.MaxInt64 {
			return KeyVal{}, false
		}
		return KeyVal{Type: k.Type, Int: k.Int + 1}, true
	case model.TypeDecimal, model.TypeFloat:
		// Find the smallest decimal step of the value's scale.
		scale := k.Dec.Exponent()
		if scale > 0 {
			scale = 0
		}
		step := decimal.New(1, scale)
		return KeyVal{Type: k.Type, Dec: k.Dec.Add(step)}, true
	case model.TypeDate:
		return KeyVal{Type: k.Type, Time: k.Time.AddDate(0, 0, 1)}, true
	case model.TypeDateTime, model.TypeTime:
		return KeyVal{Type: k.Type, Time: k.Time.Add(time.Nanosecond)}, true
	case model.TypeBytes:
		out := append([]byte(nil), k.Bytes...)
		out = append(out, 0x00)
		return KeyVal{Type: k.Type, Bytes: out}, true
	default:
		return KeyVal{Type: k.Type, Str: k.Str + "\x00"}, true
	}
}

// KeyValFromString reconstructs a KeyVal from its display form.  It is used
// when a stored difference is re-read during the stability recheck.
func KeyValFromString(t model.LogicalType, s string) KeyVal {
	switch t {
	case model.TypeInt:
		var v int64
		fmt.Sscanf(s, "%d", &v)
		return KeyVal{Type: t, Int: v}
	case model.TypeDecimal, model.TypeFloat:
		d, _ := decimal.NewFromString(s)
		return KeyVal{Type: t, Dec: d}
	case model.TypeDate:
		when, _ := time.Parse("2006-01-02", s)
		return KeyVal{Type: t, Time: when}
	case model.TypeTime:
		when, _ := time.Parse("15:04:05.999999999", s)
		return KeyVal{Type: t, Time: when}
	case model.TypeDateTime:
		when, _ := time.Parse(time.RFC3339Nano, s)
		return KeyVal{Type: t, Time: when}
	case model.TypeBytes:
		return KeyVal{Type: t, Bytes: []byte(s)}
	default:
		return KeyVal{Type: model.TypeString, Str: s}
	}
}
