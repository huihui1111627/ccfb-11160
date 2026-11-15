package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"bankcmp/internal/app"
	"bankcmp/internal/engine"
	"bankcmp/internal/model"
	"bankcmp/internal/source"
	"bankcmp/internal/verify"
)

// runCompare executes a YAML config without the web UI.  It is the command
// used to prove a migration: -verify compares the result against the planted
// manifest and exits non-zero on any mismatch.
func runCompare(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	configPath := fs.String("config", "", "task configuration YAML")
	manifestPath := fs.String("verify", "", "manifest of planted differences to verify against")
	jsonOut := fs.Bool("json", false, "print machine readable JSON")
	tableFilter := fs.String("tables", "", "comma separated table subset")
	chunkRows := fs.Int("chunk-rows", 0, "override options.chunk_rows")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "compare: -config is required")
		return 2
	}
	text, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read config: %v\n", err)
		return 2
	}
	cfg, err := app.ParseConfig(string(text))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}
	if cfg.Source.DSN == "" || cfg.Target.DSN == "" {
		fmt.Fprintln(os.Stderr, "config must set source.dsn and target.dsn")
		return 2
	}
	opts := cfg.Options
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.ChunkWorkers <= 0 {
		opts.ChunkWorkers = 1
	}
	if opts.ChunkRows <= 0 {
		opts.ChunkRows = model.DefaultOptions().ChunkRows
	}
	if opts.StatementTimeoutMS <= 0 {
		opts.StatementTimeoutMS = model.DefaultOptions().StatementTimeoutMS
	}
	if *chunkRows > 0 {
		opts.ChunkRows = *chunkRows
	}
	ctx := context.Background()
	src, err := source.Open(ctx, source.Config{Driver: cfg.Source.Driver, DSN: cfg.Source.DSN,
		Charset: cfg.Source.Charset, Timezone: "UTC", ReadOnly: true,
		MaxConns:           opts.ChunkWorkers*opts.Workers + 4,
		StatementTimeoutMS: opts.StatementTimeoutMS})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open source: %v\n", err)
		return 2
	}
	defer src.Close()
	tgt, err := source.Open(ctx, source.Config{Driver: cfg.Target.Driver, DSN: cfg.Target.DSN,
		Charset: cfg.Target.Charset, Timezone: "UTC", ReadOnly: true,
		MaxConns:           opts.ChunkWorkers*opts.Workers + 4,
		StatementTimeoutMS: opts.StatementTimeoutMS})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open target: %v\n", err)
		return 2
	}
	defer tgt.Close()

	runner := engine.NewRunner(src, tgt, engine.Options{
		ChunkWorkers: opts.ChunkWorkers, ChunkRows: opts.ChunkRows,
		DiffMaxRows: opts.DiffMaxRows, LeafRows: opts.ChunkRows,
		MaxSplitDepth: 16, Recheck: opts.Recheck,
		Settle:    time.Duration(opts.SettleMS) * time.Millisecond,
		SourceLim: engine.NewLimiter(opts.RowsPerSecond, opts.QueriesPerSec),
		TargetLim: engine.NewLimiter(opts.RowsPerSecond, opts.QueriesPerSec),
	})

	only := map[string]bool{}
	for _, t := range strings.Split(*tableFilter, ",") {
		if t = strings.TrimSpace(t); t != "" {
			only[strings.ToLower(t)] = true
		}
	}
	var results []*engine.TableResult
	start := time.Now()
	for _, spec := range cfg.Tables {
		if spec.Ignore {
			continue
		}
		if len(only) > 0 && !only[strings.ToLower(spec.Name)] {
			continue
		}
		res, err := runner.CompareSpec(ctx, spec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "table %s: %v\n", spec.Name, err)
			return 1
		}
		results = append(results, res)
	}
	elapsed := time.Since(start)

	var observed []verify.Result
	for _, res := range results {
		for _, d := range res.Diffs {
			var cols []string
			for _, c := range d.Columns {
				cols = append(cols, c.Column)
			}
			observed = append(observed, verify.Result{
				Table: res.TableName, PK: d.PK, Kind: string(d.Kind), Columns: cols,
			})
		}
	}

	printSummary(results, elapsed)
	if *jsonOut {
		printJSON(results, elapsed)
	}
	if *manifestPath != "" {
		m, err := verify.LoadManifest(*manifestPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load manifest: %v\n", err)
			return 2
		}
		rep := verify.Check(m, observed)
		fmt.Println()
		fmt.Print(rep.String())
		if !rep.OK {
			return 1
		}
	}
	for _, res := range results {
		if res.Status == model.TableFailed {
			return 1
		}
	}
	return 0
}
