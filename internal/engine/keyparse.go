package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"bankcmp/internal/canon"
	"bankcmp/internal/model"
	"bankcmp/internal/plan"
)

// keyParser turns raw key values from one side into comparable KeyVals.  It
// parses both sides into the canonical key type agreed at plan time so that a
// boundary value takes the same form everywhere.
type keyParser struct {
	keys   []plan.KeyCol
	srcLoc *time.Location
	tgtLoc *time.Location
}

func (p keyParser) parse(side plan.Side, raws []canon.Raw) ([]plan.KeyVal, error) {
	out := make([]plan.KeyVal, len(raws))
	for i, r := range raws {
		kv, err := p.parseOne(side, i, r)
		if err != nil {
			return nil, err
		}
		out[i] = kv
	}
	return out, nil
}

func (p keyParser) parseOne(side plan.Side, i int, r canon.Raw) (plan.KeyVal, error) {
	k := p.keys[i]
	if r.Null {
		return plan.KeyVal{}, fmt.Errorf("NULL value in key column %q", k.SourceName)
	}
	typ := k.Canonical
	if typ == model.TypeAuto || typ == "" {
		typ = k.SourceType
	}
	loc := p.srcLoc
	if side == plan.SideTarget {
		loc = p.tgtLoc
	}
	if loc == nil {
		loc = time.UTC
	}
	switch typ {
	case model.TypeInt:
		switch r.Kind {
		case 'i':
			return plan.KeyVal{Type: model.TypeInt, Int: r.I}, nil
		case 'f':
			if r.F == float64(int64(r.F)) {
				return plan.KeyVal{Type: model.TypeInt, Int: int64(r.F)}, nil
			}
		}
		s := strings.TrimSpace(keyText(r))
		v, err := strconv.ParseInt(strings.TrimPrefix(s, "+"), 10, 64)
		if err != nil {
			return plan.KeyVal{}, fmt.Errorf("key column %q: cannot parse %q as integer", k.SourceName, s)
		}
		return plan.KeyVal{Type: model.TypeInt, Int: v}, nil
	case model.TypeDecimal:
		d, err := decimal.NewFromString(strings.TrimSpace(keyText(r)))
		if err != nil {
			return plan.KeyVal{}, fmt.Errorf("key column %q: cannot parse %q as numeric", k.SourceName, keyText(r))
		}
		return plan.KeyVal{Type: model.TypeDecimal, Dec: d}, nil
	case model.TypeFloat:
		d, err := decimal.NewFromString(strings.TrimSpace(keyText(r)))
		if err != nil {
			return plan.KeyVal{}, fmt.Errorf("key column %q: cannot parse %q as float", k.SourceName, keyText(r))
		}
		return plan.KeyVal{Type: model.TypeFloat, Dec: d}, nil
	case model.TypeDate, model.TypeTime, model.TypeDateTime:
		if r.Kind == 't' {
			if typ == model.TypeDateTime {
				return plan.KeyVal{Type: typ, Time: r.T.UTC()}, nil
			}
			return plan.KeyVal{Type: typ, Time: r.T.In(loc)}, nil
		}
		t, err := parseKeyTime(keyText(r), typ, loc)
		if err != nil {
			return plan.KeyVal{}, fmt.Errorf("key column %q: %w", k.SourceName, err)
		}
		return plan.KeyVal{Type: typ, Time: t}, nil
	case model.TypeBytes:
		var b []byte
		if r.Kind == 'b' {
			b = r.B
		} else {
			b = []byte(keyText(r))
		}
		return plan.KeyVal{Type: model.TypeBytes, Bytes: b}, nil
	case model.TypeUUID:
		// A UUID orders by its 16 bytes.  PostgreSQL's canonical text form is
		// lowercase hex with fixed hyphens, whose lexicographic order equals the
		// byte order.  Any other spelling (uppercase, braces, no hyphens) would
		// sort differently in the database than here, silently breaking the
		// merge, so it is rejected rather than tolerated.
		raw := keyText(r)
		canon := strings.ToLower(strings.Trim(strings.TrimSpace(raw), "{}"))
		canon = strings.ReplaceAll(canon, "-", "")
		if len(canon) != 32 || !isHex(canon) {
			return plan.KeyVal{}, fmt.Errorf("key column %q: %q is not a canonical UUID", k.SourceName, raw)
		}
		canonical := formatUUID(canon)
		if raw != canonical {
			return plan.KeyVal{}, fmt.Errorf("key column %q: UUID %q is not in canonical lowercase form; the database's ordering of the stored text would not match the comparison. Store canonical UUID text, or use an integer/text key", k.SourceName, raw)
		}
		return plan.KeyVal{Type: model.TypeString, Str: canonical}, nil
	default:
		return plan.KeyVal{Type: model.TypeString, Str: keyText(r)}, nil
	}
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// formatUUID renders 32 hex digits in canonical 8-4-4-4-12 form.
func formatUUID(h string) string {
	if len(h) != 32 {
		return h
	}
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func keyText(r canon.Raw) string {
	switch r.Kind {
	case 's':
		return r.S
	case 'b':
		return string(r.B)
	case 'i':
		return strconv.FormatInt(r.I, 10)
	case 'f':
		return strconv.FormatFloat(r.F, 'g', -1, 64)
	case 't':
		return r.T.Format(time.RFC3339Nano)
	default:
		return r.S
	}
}

func parseKeyTime(s string, typ model.LogicalType, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if loc == nil {
		loc = time.UTC
	}
	var layouts []string
	switch typ {
	case model.TypeDate:
		layouts = []string{"2006-01-02", "2006/01/02", "20060102"}
	case model.TypeTime:
		layouts = []string{"15:04:05.999999999", "15:04:05", "15:04", "150405"}
	default:
		layouts = []string{time.RFC3339Nano, time.RFC3339,
			"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05",
			"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05",
			"2006-01-02", "20060102150405"}
	}
	for _, layout := range layouts {
		if strings.Contains(layout, "Z07") {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC(), nil
			}
			continue
		}
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as %s", s, typ)
}
