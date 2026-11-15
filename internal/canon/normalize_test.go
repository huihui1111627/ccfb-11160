package canon

import (
	"testing"
	"time"

	"bankcmp/internal/model"
)

func mustNorm(t *testing.T, o Options) *Normalizer {
	t.Helper()
	n, err := NewNormalizer(o)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDecimalTrailingZeros(t *testing.T) {
	n := mustNorm(t, Options{Type: model.TypeDecimal})
	cases := [][2]string{
		{"123.4500", "123.45"}, {"123.45", "123.45"}, {"0.000", "0"},
		{"-0.00", "0"}, {"1e3", "1000"}, {"100.00", "100"},
	}
	for _, c := range cases {
		v, err := n.Normalize(Str(c[0]))
		if err != nil {
			t.Fatalf("%s: %v", c[0], err)
		}
		if got := string(v.Raw); got != c[1] {
			t.Errorf("%s -> %s, want %s", c[0], got, c[1])
		}
	}
	if !n.Equal(mustVal(t, n, "123.4500"), mustVal(t, n, "123.45")) {
		t.Error("123.4500 should equal 123.45")
	}
}

func mustVal(t *testing.T, n *Normalizer, s string) Value {
	t.Helper()
	v, err := n.Normalize(Str(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestScaleRounding(t *testing.T) {
	n := mustNorm(t, Options{Type: model.TypeDecimal, Scale: 2})
	if !n.Equal(mustVal(t, n, "123.454"), mustVal(t, n, "123.45")) {
		t.Error("scale 2 should ignore the third decimal")
	}
	if n.Equal(mustVal(t, n, "123.46"), mustVal(t, n, "123.45")) {
		t.Error("different values at scale 2 must differ")
	}
}

func TestGBKToUTF8(t *testing.T) {
	gbkBytes := []byte{0xd5, 0xc5, 0xc8, 0xfd} // 张三 in GBK
	srcN := mustNorm(t, Options{Type: model.TypeString, Charset: "gbk"})
	tgtN := mustNorm(t, Options{Type: model.TypeString})
	a, err := srcN.Normalize(Bytes(gbkBytes))
	if err != nil {
		t.Fatal(err)
	}
	b := mustVal(t, tgtN, "张三")
	if !srcN.Equal(a, b) {
		t.Errorf("GBK 张三 should equal UTF-8 张三: %q vs %q", a.Raw, b.Raw)
	}
}

func TestDateFormats(t *testing.T) {
	n := mustNorm(t, Options{Type: model.TypeDate})
	want := "2024-01-02"
	for _, s := range []string{"2024-01-02", "2024/01/02", "20240102", "2024-1-2"} {
		v, err := n.Normalize(Str(s))
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if string(v.Raw) != want {
			t.Errorf("%s -> %s, want %s", s, v.Raw, want)
		}
	}
	// A time.Time from the driver (SQLite DATE column) must land on the same day.
	v, err := n.Normalize(Time(time.Date(2024, 1, 2, 13, 5, 0, 0, time.FixedZone("x", 3600))))
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Raw) != want {
		t.Errorf("time.Time -> %s, want %s", v.Raw, want)
	}
}

func TestDateTimeTimezone(t *testing.T) {
	shanghai := time.FixedZone("CST", 8*3600)
	n := mustNorm(t, Options{Type: model.TypeDateTime, Timezone: shanghai})
	// The same instant expressed in Beijing time and UTC must compare equal.
	naive, err := n.Normalize(Str("2024-01-02 03:04:05"))
	if err != nil {
		t.Fatal(err)
	}
	utc := mustNorm(t, Options{Type: model.TypeDateTime, Timezone: time.UTC})
	instant, err := utc.Normalize(Time(time.Date(2024, 1, 1, 19, 4, 5, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	if !n.Equal(naive, instant) {
		t.Errorf("naive Beijing time should equal the same instant in UTC: %s vs %s", naive.Raw, instant.Raw)
	}
}

func TestWhitespaceAndCase(t *testing.T) {
	n := mustNorm(t, Options{Type: model.TypeString, TrimSpace: true, CollapseSpace: true, CaseFold: true})
	if !n.Equal(mustVal(t, n, "  Hello   World "), mustVal(t, n, "hello world")) {
		t.Error("trim/collapse/casefold should make these equal")
	}
}

func TestNullEqualsEmpty(t *testing.T) {
	n := mustNorm(t, Options{Type: model.TypeString, NullEqualsEmpty: true})
	if !n.Equal(Value{Null: true}, mustVal(t, n, "")) {
		t.Error("NULL should equal empty string when configured")
	}
	n2 := mustNorm(t, Options{Type: model.TypeString})
	if n2.Equal(Value{Null: true}, mustVal(t, n2, "")) {
		t.Error("NULL should not equal empty string by default")
	}
}

func TestBoolSpellings(t *testing.T) {
	n := mustNorm(t, Options{Type: model.TypeBool})
	tv := mustVal(t, n, "T")
	iv, err := n.Normalize(Int(1))
	if err != nil {
		t.Fatal(err)
	}
	if !n.Equal(tv, iv) {
		t.Error("'T' and 1 should both be true")
	}
	fv := mustVal(t, n, "false")
	zv, _ := n.Normalize(Int(0))
	if !n.Equal(fv, zv) {
		t.Error("'false' and 0 should both be false")
	}
}

func TestUnknownCharsetRejected(t *testing.T) {
	if _, err := NewNormalizer(Options{Type: model.TypeString, Charset: "klingon"}); err == nil {
		t.Fatal("unknown charset must be rejected, not silently ignored")
	}
}

func TestFloatTolerance(t *testing.T) {
	n := mustNorm(t, Options{Type: model.TypeFloat, FloatTolerance: 1e-9})
	a, _ := n.Normalize(Float(1.0000000001))
	b, _ := n.Normalize(Float(1.0))
	if !n.Equal(a, b) {
		t.Error("floats within tolerance should compare equal")
	}
	c, _ := n.Normalize(Float(1.01))
	if n.Equal(a, c) {
		t.Error("floats outside tolerance must differ")
	}
}
