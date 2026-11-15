package canon

import (
	"fmt"
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// charsets maps common aliases to a text encoding.  A nil encoding means the
// text is already UTF-8 and needs no transformation.
var charsets = map[string]encoding.Encoding{
	"":             nil,
	"utf8":         nil,
	"utf-8":        nil,
	"unicode":      nil,
	"gbk":          simplifiedchinese.GBK,
	"gb2312":       simplifiedchinese.GBK,
	"cp936":        simplifiedchinese.GBK,
	"gb18030":      simplifiedchinese.GB18030,
	"big5":         traditionalchinese.Big5,
	"cp950":        traditionalchinese.Big5,
	"big5-hkscs":   traditionalchinese.Big5,
	"sjis":         japanese.ShiftJIS,
	"shift-jis":    japanese.ShiftJIS,
	"shift_jis":    japanese.ShiftJIS,
	"cp932":        japanese.ShiftJIS,
	"eucjp":        japanese.EUCJP,
	"euc-jp":       japanese.EUCJP,
	"euckr":        korean.EUCKR,
	"euc-kr":       korean.EUCKR,
	"latin1":       charmap.ISO8859_1,
	"iso-8859-1":   charmap.ISO8859_1,
	"cp1252":       charmap.Windows1252,
	"windows-1252": charmap.Windows1252,
}

// LookupCharset resolves a charset name (case insensitive).
func LookupCharset(name string) (encoding.Encoding, bool) {
	e, ok := charsets[strings.ToLower(strings.TrimSpace(name))]
	if ok {
		return e, true
	}
	// allow a charset name that x/text knows through its iana index would be
	// nicer, but the explicit table above covers the bank legacy set; treat
	// an unknown name as a configuration error elsewhere.
	return nil, false
}

// CharsetName normalises a charset alias for display.
func CharsetName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "", "utf8", "utf-8", "unicode":
		return "utf-8"
	case "gb2312", "cp936", "gbk":
		return "gbk"
	}
	return n
}

var _ = fmt.Sprintf
