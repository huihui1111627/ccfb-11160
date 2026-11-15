package verify_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"bankcmp/internal/engine"
	"bankcmp/internal/model"
	"bankcmp/internal/seed"
	"bankcmp/internal/source"
	"bankcmp/internal/verify"

	"bankcmp/internal/app"
)

// TestSeededPairReproducesManifestExactly is the ground truth test: seed a
// pair with known planted differences, compare it, and require the observed
// list to match the manifest exactly - no extra rows, no missing rows.
func TestSeededPairReproducesManifestExactly(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := seed.SeedAll(dir, 3000, 7, manifestPath); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := app.ParseConfig(string(text))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src, err := source.Open(ctx, source.Config{Driver: cfg.Source.Driver, DSN: cfg.Source.DSN,
		Charset: cfg.Source.Charset, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	tgt, err := source.Open(ctx, source.Config{Driver: cfg.Target.Driver, DSN: cfg.Target.DSN,
		ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tgt.Close()
	runner := engine.NewRunner(src, tgt, engine.Options{ChunkRows: 500, ChunkWorkers: 3, Recheck: false})
	var observed []verify.Result
	for _, spec := range cfg.Tables {
		res, err := runner.CompareSpec(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Diffs {
			var cols []string
			for _, c := range d.Columns {
				cols = append(cols, c.Column)
			}
			observed = append(observed, verify.Result{Table: res.TableName, PK: d.PK, Kind: string(d.Kind), Columns: cols})
		}
	}
	m, err := verify.LoadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	rep := verify.Check(m, observed)
	if !rep.OK {
		t.Fatalf("planted differences not reproduced:\n%s", rep.String())
	}
	t.Logf("%d planted differences reproduced exactly across %d tables", rep.Expected, len(m.Tables))
	_ = model.DriverSQLite
}
