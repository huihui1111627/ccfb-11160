package source

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	sqlite3 "github.com/mattn/go-sqlite3"
	"golang.org/x/text/transform"

	"bankcmp/internal/canon"
)

// sqliteDriverName is a private driver registration that adds summary UDFs so
// that large columns can be hashed inside SQLite instead of being shipped to
// the comparison engine.
const sqliteDriverName = "sqlite3_bankcmp"

func init() {
	sql.Register(sqliteDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			if err := conn.RegisterFunc("bc_sha256", digestValue, true); err != nil {
				return err
			}
			if err := conn.RegisterFunc("bc_sha256_text", digestText, true); err != nil {
				return err
			}
			return nil
		},
	})
}

// digestValue returns the lowercase hex SHA-256 of a value's bytes.
func digestValue(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	var b []byte
	switch x := v.(type) {
	case string:
		b = []byte(x)
	case []byte:
		b = x
	case int64:
		b = []byte(fmt.Sprintf("%d", x))
	case float64:
		b = []byte(fmt.Sprintf("%v", x))
	default:
		b = []byte(fmt.Sprint(x))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// digestText decodes raw bytes from a legacy charset into UTF-8 before
// hashing, so a GBK column and a UTF-8 column holding the same characters
// produce the same digest.
func digestText(v any, charset string) (any, error) {
	if v == nil {
		return nil, nil
	}
	var b []byte
	switch x := v.(type) {
	case string:
		b = []byte(x)
	case []byte:
		b = x
	default:
		b = []byte(fmt.Sprint(x))
	}
	enc, ok := canon.LookupCharset(charset)
	if ok && enc != nil {
		out, _, err := transform.Bytes(enc.NewDecoder(), b)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", charset, err)
		}
		b = out
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func sqliteQuote(id string) string {
	parts := strings.Split(id, ".")
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(strings.Trim(p, `"`), `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}
