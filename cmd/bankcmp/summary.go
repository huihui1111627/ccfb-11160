package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"bankcmp/internal/engine"
)

// printSummary writes the report table: rows, elapsed time and difference
// counts per table, as required by the migration sign off.
func printSummary(results []*engine.TableResult, elapsed time.Duration) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TABLE\tSTATUS\tSOURCE_ROWS\tTARGET_ROWS\tDIFFS\tMISSING\tEXTRA\tMODIFIED\tSTABLE\tTRANSIENT\tCHUNKS\tDURATION")
	sort.SliceStable(results, func(i, j int) bool { return results[i].TableName < results[j].TableName })
	var totalSrc, totalTgt, totalDiffs int64
	for _, r := range results {
		var missing, extra, modified int64
		for _, d := range r.Diffs {
			switch d.Kind {
			case "missing":
				missing++
			case "extra":
				extra++
			case "modified":
				modified++
			}
		}
		top := 0
		for _, c := range r.Chunks {
			if c.Depth == 0 {
				top++
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
			r.TableName, r.Status, r.SourceRows, r.TargetRows, len(r.Diffs),
			missing, extra, modified, r.Stable, r.Transient, top,
			r.Duration.Round(time.Millisecond))
		totalSrc += r.SourceRows
		totalTgt += r.TargetRows
		totalDiffs += int64(len(r.Diffs))
	}
	w.Flush()
	fmt.Printf("\ntotal: %d source rows, %d target rows, %d difference(s) in %s\n",
		totalSrc, totalTgt, totalDiffs, elapsed.Round(time.Millisecond))
}

func printJSON(results []*engine.TableResult, elapsed time.Duration) {
	type tableOut struct {
		Table      string `json:"table"`
		Status     string `json:"status"`
		SourceRows int64  `json:"source_rows"`
		TargetRows int64  `json:"target_rows"`
		Diffs      int    `json:"diffs"`
		Stable     int64  `json:"stable"`
		Transient  int64  `json:"transient"`
		DurationMS int64  `json:"duration_ms"`
		Error      string `json:"error,omitempty"`
	}
	out := struct {
		Tables     []tableOut `json:"tables"`
		DurationMS int64      `json:"duration_ms"`
	}{DurationMS: elapsed.Milliseconds()}
	for _, r := range results {
		out.Tables = append(out.Tables, tableOut{
			Table: r.TableName, Status: string(r.Status), SourceRows: r.SourceRows,
			TargetRows: r.TargetRows, Diffs: len(r.Diffs), Stable: r.Stable,
			Transient: r.Transient, DurationMS: r.Duration.Milliseconds(), Error: r.Error,
		})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
