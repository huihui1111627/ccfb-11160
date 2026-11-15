// Command seedtest builds a pair of databases representing a legacy bank
// schema and its migrated copy, and writes a manifest of the differences a
// correct comparison must report - exactly, with nothing extra and nothing
// missing.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"

	"bankcmp/internal/seed"
)

func main() {
	dir := flag.String("dir", "testdata", "directory for the generated databases")
	rows := flag.Int("rows", 200000, "base rows per table")
	seedVal := flag.Int64("seed", 42, "random seed for planted differences")
	manifest := flag.String("manifest", "", "manifest path (default <dir>/manifest.json)")
	pgAdmin := flag.String("pg", "", "seed a PostgreSQL pair instead, using this admin DSN")
	pgSrc := flag.String("pg-source", "bankcmp_src", "PostgreSQL source database name")
	pgTgt := flag.String("pg-target", "bankcmp_tgt", "PostgreSQL target database name")
	cfgPath := flag.String("config", "", "config output path (default <dir>/config.yaml)")
	flag.Parse()
	mp := *manifest
	if mp == "" {
		mp = filepath.Join(*dir, "manifest.json")
	}
	if *pgAdmin != "" {
		cp := *cfgPath
		if cp == "" {
			cp = filepath.Join(*dir, "config.yaml")
		}
		if err := os.MkdirAll(*dir, 0o755); err != nil {
			log.Fatal(err)
		}
		if err := seed.PostgresPair(context.Background(), *pgAdmin, *pgSrc, *pgTgt, *rows, *seedVal, mp, cp); err != nil {
			log.Fatal(err)
		}
		log.Printf("PostgreSQL pair seeded; manifest %s, config %s", mp, cp)
		return
	}
	if err := seed.SeedAll(*dir, *rows, *seedVal, mp); err != nil {
		log.Fatal(err)
	}
}
