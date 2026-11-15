package verify_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"bankcmp/internal/app"
	"bankcmp/internal/engine"
	"bankcmp/internal/model"
	"bankcmp/internal/plan"
	"bankcmp/internal/seed"
	"bankcmp/internal/source"
	"bankcmp/internal/sqlgen"
	"bankcmp/internal/verify"
)

// TestRepairConverges is the full loop: seed planted differences, compare,
// generate repair SQL from freshly read full rows (including digest columns),
// apply it to the target, compare again, and require zero differences.
func TestRepairConverges(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := seed.SeedAll(dir, 400, 11, manifestPath); err != nil {
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
	manifest, err := verify.LoadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// First pass: the difference list must match the manifest.
	first, observed := compareAll(t, cfg, dir)
	if rep := verify.Check(manifest, observed); !rep.OK {
		t.Fatalf("first pass did not match the manifest:\n%s", rep.String())
	}

	// Generate and apply repairs using the same code path as the API.
	tgt, err := source.Open(ctx, source.Config{Driver: cfg.Target.Driver, DSN: cfg.Target.DSN})
	if err != nil {
		t.Fatal(err)
	}
	defer tgt.Close()
	src, err := source.Open(ctx, source.Config{Driver: cfg.Source.Driver, DSN: cfg.Source.DSN,
		Charset: cfg.Source.Charset, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	runner := engine.NewRunner(src, tgt, engine.Options{})
	totalApplied := 0
	for _, spec := range cfg.Tables {
		res := first[spec.Name]
		if len(res.Diffs) == 0 {
			continue
		}
		repairSpec := withoutDigest(spec)
		ss, err := src.Introspect(ctx, repairSpec.Source())
		if err != nil {
			t.Fatal(err)
		}
		ts, err := tgt.Introspect(ctx, repairSpec.Target())
		if err != nil {
			t.Fatal(err)
		}
		tp, err := plan.Build(repairSpec, src, tgt, ss, ts, cfg.Options)
		if err != nil {
			t.Fatal(err)
		}
		in := sqlgen.Input{SourceRows: map[string]sqlgen.SourceRow{}, TargetRows: map[string]sqlgen.SourceRow{}}
		var needSrc, needTgt []model.Diff
		for _, d := range res.Diffs {
			switch d.Kind {
			case model.KindMissing:
				needSrc = append(needSrc, d)
			case model.KindExtra:
				needTgt = append(needTgt, d)
			case model.KindModified:
				needSrc = append(needSrc, d)
				needTgt = append(needTgt, d)
			}
		}
		if len(needSrc) > 0 {
			rows, err := runner.FetchFullRows(ctx, tp, plan.SideSource, needSrc)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range rows {
				in.SourceRows[k] = sqlgen.SourceRow{Key: v.Key, Vals: v.Vals}
			}
		}
		if len(needTgt) > 0 {
			rows, err := runner.FetchFullRows(ctx, tp, plan.SideTarget, needTgt)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range rows {
				in.TargetRows[k] = sqlgen.SourceRow{Key: v.Key, Vals: v.Vals}
			}
		}
		b, err := sqlgen.Generate(tp, tgt.Dial, res.Diffs, in, sqlgen.DefaultOptions())
		if err != nil {
			t.Fatalf("%s repair: %v", spec.Name, err)
		}
		// Apply through the target.
		for _, stmt := range splitSQL(b.SQL) {
			if err := tgt.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s apply %q: %v", spec.Name, stmt, err)
			}
			totalApplied++
		}
	}
	if totalApplied == 0 {
		t.Fatal("no repair statements were generated")
	}

	// Second pass: everything must be equal now.
	second, _ := compareAll(t, cfg, dir)
	for name, res := range second {
		if len(res.Diffs) != 0 {
			t.Fatalf("table %s still has %d differences after repair: %+v", name, len(res.Diffs), res.Diffs[:min(3, len(res.Diffs))])
		}
		if res.Status != model.TableEqual {
			t.Fatalf("table %s status %s after repair", name, res.Status)
		}
	}
	t.Logf("applied %d statements; all tables equal afterwards", totalApplied)
}

func compareAll(t *testing.T, cfg *model.Config, dir string) (map[string]*engine.TableResult, []verify.Result) {
	t.Helper()
	ctx := context.Background()
	src, err := source.Open(ctx, source.Config{Driver: cfg.Source.Driver, DSN: cfg.Source.DSN,
		Charset: cfg.Source.Charset, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	tgt, err := source.Open(ctx, source.Config{Driver: cfg.Target.Driver, DSN: cfg.Target.DSN, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tgt.Close()
	runner := engine.NewRunner(src, tgt, engine.Options{ChunkRows: 200, ChunkWorkers: 2, Recheck: false})
	out := map[string]*engine.TableResult{}
	var observed []verify.Result
	for _, spec := range cfg.Tables {
		res, err := runner.CompareSpec(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		out[spec.Name] = res
		for _, d := range res.Diffs {
			var cols []string
			for _, c := range d.Columns {
				cols = append(cols, c.Column)
			}
			observed = append(observed, verify.Result{Table: res.TableName, PK: d.PK, Kind: string(d.Kind), Columns: cols})
		}
	}
	return out, observed
}

// withoutDigest mirrors the API's repair path.
func withoutDigest(spec model.TableSpec) model.TableSpec {
	out := spec
	out.Columns = append([]model.ColumnSpec(nil), spec.Columns...)
	for i := range out.Columns {
		out.Columns[i].Digest = false
	}
	return out
}

func splitSQL(text string) []string {
	var out []string
	start := 0
	inString := false
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\'':
			if inString && i+1 < len(text) && text[i+1] == '\'' {
				i++
				continue
			}
			inString = !inString
		case ';':
			if !inString {
				out = append(out, text[start:i])
				start = i + 1
			}
		}
	}
	return out
}
