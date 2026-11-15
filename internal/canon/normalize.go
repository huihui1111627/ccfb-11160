package canon

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/shopspring/decimal"
	"golang.org/x/text/encoding"
	"golang.org/x/text/transform"

	"bankcmp/internal/model"
)

// Value is the canonical form of a value.
type Value struct {
	// Null reports SQL NULL.
	Null bool
	// Raw is the canonical byte representation: canonical decimal text,
	// base-10 integers, UTC RFC3339Nano timestamps, UTF-8 strings, raw bytes,
	// or the hex digest for digest-mode columns.
	Raw []byte
	// Float is set when the logical type is float.
	Float float64
	// IsFloat reports whether Float is meaningful.
	IsFloat bool
}

// String returns the canonical text form.
func (v Value) String() string {
	if v.Null {
		return ""
	}
	return string(v.Raw)
}

// Bytes returns the canonical bytes.
func (v Value) Bytes() []byte {
	if v.Null {
		return nil
	}
	return v.Raw
}

// Empty reports whether the value is NULL.
func (v Value) IsNull() bool { return v.Null }

// displayLimit caps the canonical text kept for a single value in previews.
const displayLimit = 512

// Options is the per column normalisation policy.
type Options struct {
	Type            model.LogicalType
	Charset         string
	Scale           int32
	FloatDigits     int
	FloatTolerance  float64
	TrimSpace       bool
	CollapseSpace   bool
	CaseFold        bool
	NullEqualsEmpty bool
	Layouts         []string
	Timezone        *time.Location
	BoolTrue        []string
	BoolFalse       []string
	// Digest marks a digest-mode column.  The extractor already produced the
	// digest as text, so Compare just compares it as a string.
	Digest bool
}

// Normalizer canonicalises one column.
type Normalizer struct {
	Opt  Options
	enc  encoding.Encoding
	loc  *time.Location
	kind model.LogicalType
}

// NewNormalizer builds a normalizer for a column.  An unknown charset is
// reported as an error so a misconfiguration cannot silently produce false
// differences.
func NewNormalizer(o Options) (*Normalizer, error) {
	n := &Normalizer{Opt: o}
	if n.Opt.Type == model.TypeAuto {
		n.Opt.Type = model.TypeString
	}
	switch n.Opt.Type {
	case model.TypeInt, model.TypeDecimal, model.TypeFloat, model.TypeString,
		model.TypeBytes, model.TypeBool, model.TypeDate, model.TypeDateTime,
		model.TypeTime, model.TypeUUID:
		n.kind = n.Opt.Type
	default:
		return nil, fmt.Errorf("unknown logical type %q", n.Opt.Type)
	}
	if cs := strings.ToLower(strings.TrimSpace(o.Charset)); cs != "" {
		e, ok := LookupCharset(cs)
		if !ok {
			return nil, fmt.Errorf("unsupported charset %q", o.Charset)
		}
		n.enc = e
	}
	n.loc = o.Timezone
	if n.loc == nil {
		n.loc = time.UTC
	}
	return n, nil
}

// Kind returns the resolved logical type.
func (n *Normalizer) Kind() model.LogicalType { return n.kind }

// decodeText turns raw bytes/text into UTF-8 according to the column charset.
func (n *Normalizer) decodeText(r Raw) (string, error) {
	var b []byte
	switch r.Kind {
	case 's':
		b = []byte(r.S)
	case 'b':
		b = r.B
	case 'i':
		return strconv.FormatInt(r.I, 10), nil
	case 'f':
		return strconv.FormatFloat(r.F, 'g', -1, 64), nil
	case 't':
		return r.T.Format(time.RFC3339Nano), nil
	default:
		b = []byte(r.S)
	}
	if n.enc == nil {
		return string(b), nil
	}
	out, _, err := transform.Bytes(n.enc.NewDecoder(), b)
	if err != nil {
		return "", fmt.Errorf("decode %s text: %w", n.Opt.Charset, err)
	}
	return string(out), nil
}

// Normalize converts a raw driver value into its canonical form.
func (n *Normalizer) Normalize(r Raw) (Value, error) {
	if r.Null {
		if n.Opt.NullEqualsEmpty {
			return Value{Raw: []byte{}}, nil
		}
		return Value{Null: true}, nil
	}
	switch n.kind {
	case model.TypeString, model.TypeUUID:
		s, err := n.decodeText(r)
		if err != nil {
			return Value{}, err
		}
		if n.Opt.NullEqualsEmpty && s == "" {
			return Value{Null: true}, nil
		}
		s = n.cleanString(s)
		if n.kind == model.TypeUUID {
			s = strings.ToLower(strings.ReplaceAll(strings.Trim(s, "{}"), "-", ""))
		}
		return Value{Raw: []byte(s)}, nil
	case model.TypeBytes:
		var b []byte
		switch r.Kind {
		case 'b':
			b = append([]byte(nil), r.B...)
		case 's':
			b = []byte(r.S)
		case 'i':
			b = []byte(strconv.FormatInt(r.I, 10))
		default:
			b = []byte(r.S)
		}
		if n.Opt.NullEqualsEmpty && len(b) == 0 {
			return Value{Null: true}, nil
		}
		if n.enc != nil {
			// A binary column stored in a legacy charset: canonicalise its
			// text decoding so that equal characters compare equal.
			s, err := n.decodeText(r)
			if err != nil {
				return Value{}, err
			}
			b = []byte(s)
		}
		return Value{Raw: b}, nil
	case model.TypeInt:
		i, err := parseInteger(r)
		if err != nil {
			return Value{}, err
		}
		return Value{Raw: []byte(strconv.FormatInt(i, 10))}, nil
	case model.TypeBool:
		b, err := n.parseBool(r)
		if err != nil {
			return Value{}, err
		}
		if b {
			return Value{Raw: []byte("1")}, nil
		}
		return Value{Raw: []byte("0")}, nil
	case model.TypeDecimal:
		f, err := parseDecimal(strings.TrimSpace(rawText(r)))
		if err != nil {
			return Value{}, err
		}
		if n.Opt.Scale > 0 || n.Opt.Scale < 0 {
			f = f.Round(n.Opt.Scale)
		}
		return Value{Raw: []byte(canonicalDecimal(f))}, nil
	case model.TypeFloat:
		f, err := parseFloat(rawText(r))
		if err != nil {
			return Value{}, err
		}
		return Value{Raw: []byte(canonicalFloat(f, n.Opt.FloatDigits)), Float: f, IsFloat: true}, nil
	case model.TypeDate:
		t, err := n.parseTime(r, true)
		if err != nil {
			return Value{}, err
		}
		return Value{Raw: []byte(t.Format("2006-01-02"))}, nil
	case model.TypeTime:
		t, err := n.parseTime(r, false)
		if err != nil {
			return Value{}, err
		}
		return Value{Raw: []byte(t.Format("15:04:05.999999999"))}, nil
	case model.TypeDateTime:
		t, err := n.parseTime(r, false)
		if err != nil {
			return Value{}, err
		}
		return Value{Raw: []byte(t.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))}, nil
	}
	return Value{}, fmt.Errorf("unhandled logical type %q", n.kind)
}

// cleanString applies whitespace and case normalisation.
func (n *Normalizer) cleanString(s string) string {
	if n.Opt.TrimSpace || n.Opt.CollapseSpace {
		s = strings.TrimFunc(s, unicode.IsSpace)
	}
	if n.Opt.CollapseSpace {
		var b strings.Builder
		b.Grow(len(s))
		prevSpace := false
		for _, r := range s {
			if unicode.IsSpace(r) {
				if !prevSpace {
					b.WriteByte(' ')
				}
				prevSpace = true
				continue
			}
			prevSpace = false
			b.WriteRune(r)
		}
		s = b.String()
	}
	if n.Opt.CaseFold {
		s = strings.ToLower(s)
	}
	return s
}

// Equal compares two canonical values with the column policy applied.
func (n *Normalizer) Equal(a, b Value) bool {
	if a.Null || b.Null {
		if a.Null && b.Null {
			return true
		}
		if n.Opt.NullEqualsEmpty {
			empty := func(v Value) bool { return !v.Null && len(v.Raw) == 0 }
			return (a.Null && empty(b)) || (b.Null && empty(a))
		}
		return false
	}
	if n.kind == model.TypeFloat {
		if n.Opt.FloatTolerance > 0 {
			diff := math.Abs(a.Float - b.Float)
			scale := math.Max(math.Abs(a.Float), math.Abs(b.Float))
			if diff <= n.Opt.FloatTolerance || (scale > 0 && diff/scale <= n.Opt.FloatTolerance) {
				return true
			}
		}
	}
	return bytes.Equal(a.Raw, b.Raw)
}

func rawText(r Raw) string {
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

func parseInteger(r Raw) (int64, error) {
	switch r.Kind {
	case 'i':
		return r.I, nil
	case 'f':
		if r.F == math.Trunc(r.F) && math.Abs(r.F) < 9.2e18 {
			return int64(r.F), nil
		}
		return 0, fmt.Errorf("non-integral float %v in integer column", r.F)
	}
	s := strings.TrimSpace(rawText(r))
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	return strconv.ParseInt(s, 10, 64)
}

func parseDecimal(s string) (decimal.Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return decimal.Decimal{}, fmt.Errorf("empty decimal")
	}
	// Trim a leading '+' which shopspring accepts, but be explicit.
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("parse decimal %q: %w", s, err)
	}
	return d, nil
}

func parseFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty float")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse float %q: %w", s, err)
	}
	return f, nil
}

// canonicalDecimal renders a decimal with trailing fractional zeros trimmed,
// always using plain decimal notation (never exponent), and without "-0".
func canonicalDecimal(d decimal.Decimal) string {
	if d.IsZero() {
		return "0"
	}
	s := d.String()
	if strings.Contains(s, "E") || strings.Contains(s, "e") {
		s = d.StringFixed(d.Exponent() * -1)
	}
	return s
}

// canonicalFloat renders a float with round trip precision, normalising
// -0 to 0 and NaN to a stable token.
func canonicalFloat(f float64, digits int) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if f == 0 {
		return "0"
	}
	if digits > 0 {
		return strconv.FormatFloat(f, 'g', digits, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func (n *Normalizer) parseBool(r Raw) (bool, error) {
	if r.Kind == 'i' {
		return r.I != 0, nil
	}
	if r.Kind == 'f' {
		return r.F != 0, nil
	}
	s := strings.ToLower(strings.TrimSpace(rawText(r)))
	trueSet := n.Opt.BoolTrue
	falseSet := n.Opt.BoolFalse
	if len(trueSet) == 0 {
		trueSet = []string{"1", "t", "true", "y", "yes", "on"}
	}
	if len(falseSet) == 0 {
		falseSet = []string{"0", "f", "false", "n", "no", "off"}
	}
	for _, t := range trueSet {
		if s == strings.ToLower(t) {
			return true, nil
		}
	}
	for _, f := range falseSet {
		if s == strings.ToLower(f) {
			return false, nil
		}
	}
	return false, fmt.Errorf("cannot interpret %q as boolean", s)
}

var defaultDateLayouts = []string{
	"2006-01-02",
	"2006/01/02",
	"20060102",
	"02/01/2006",
	"2006-1-2",
	"2006/1/2",
	"2006年01月02日",
	"2006年1月2日",
}

var defaultDateTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006/01/02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"20060102150405",
	"20060102",
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"Mon Jan _2 15:04:05 2006",
	"2006-01-02 15:04:05 +0800",
}

var defaultTimeLayouts = []string{
	"15:04:05.999999999",
	"15:04:05",
	"15:04",
	"150405",
}

func (n *Normalizer) parseTime(r Raw, dateOnly bool) (time.Time, error) {
	if r.Kind == 't' {
		t := r.T
		if dateOnly {
			y, m, d := t.In(n.loc).Date()
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
		}
		return t, nil
	}
	if r.Kind == 'i' && dateOnly {
		// yyyymmdd stored as an integer
		s := strconv.FormatInt(r.I, 10)
		if len(s) == 8 {
			if t, err := time.ParseInLocation("20060102", s, n.loc); err == nil {
				return t, nil
			}
		}
	}
	s := strings.TrimSpace(rawText(r))
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date/time")
	}
	if epoch, err := strconv.ParseInt(s, 10, 64); err == nil {
		if len(s) >= 9 {
			// unix seconds
			return time.Unix(epoch, 0).In(n.loc), nil
		}
	}
	layouts := n.Opt.Layouts
	var defaults []string
	switch n.kind {
	case model.TypeDate:
		defaults = defaultDateLayouts
	case model.TypeTime:
		defaults = defaultTimeLayouts
	default:
		defaults = defaultDateTimeLayouts
	}
	tryLayouts := append(append([]string{}, layouts...), defaults...)
	for _, layout := range tryLayouts {
		var (
			t   time.Time
			err error
		)
		if strings.Contains(layout, "Z07") || strings.Contains(layout, "MST") {
			t, err = time.Parse(layout, s)
		} else {
			t, err = time.ParseInLocation(layout, s, n.loc)
		}
		if err == nil {
			if dateOnly {
				y, m, d := t.Date()
				return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as %s", s, n.kind)
}

// Excerpt returns a display string for a canonical value, truncated for the
// UI.  Binary values are shown as hex.
func Excerpt(v Value, binary bool, limit int) string {
	if v.Null {
		return "NULL"
	}
	if limit <= 0 {
		limit = displayLimit
	}
	if binary {
		return hex.EncodeToString(v.Raw[:min(len(v.Raw), limit)])
	}
	s := string(v.Raw)
	if len(s) > limit {
		cut := limit
		for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
			cut--
		}
		return s[:cut] + "…"
	}
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
