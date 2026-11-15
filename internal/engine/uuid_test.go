package engine

import (
	"context"
	"testing"

	"strconv"
	"time"

	"bankcmp/internal/model"
	"bankcmp/internal/source"
)

func TestUUIDKeyPostgres(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	base := timeNowNano()
	src := newPGDatabase(t, dsn, "bankcmp_uuid_s_"+base)
	tgt := newPGDatabase(t, dsn, "bankcmp_uuid_t_"+base)
	for _, db := range []*source.Database{src, tgt} {
		if err := db.Exec(context.Background(), `CREATE TABLE u (id uuid PRIMARY KEY, v text)`); err != nil {
			t.Fatal(err)
		}
	}
	ids := []string{
		"00000000-0000-0000-0000-000000000001",
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"6ba7b811-9dad-11d1-80b4-00c04fd430c8",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	}
	for _, id := range ids {
		for _, db := range []*source.Database{src, tgt} {
			if err := db.Exec(context.Background(), `INSERT INTO u VALUES ($1,$2)`, id, "v"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tgt.Exec(context.Background(), `DELETE FROM u WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	spec := model.TableSpec{Name: "u"}
	res := compareAcross(t, src, tgt, spec)
	if len(res.Diffs) != 1 || res.Diffs[0].Kind != model.KindMissing || res.Diffs[0].PK != ids[1] {
		t.Fatalf("uuid key diffs: %+v", res.Diffs)
	}
}

func timeNowNano() string { return fmtNano() }

func fmtNano() string {
	return strconv.FormatInt(time.Now().UnixNano()%1e9, 10)
}
