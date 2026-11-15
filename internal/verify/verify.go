// Package verify checks a comparison result against the planted manifest.
// It is the ground truth harness: a single extra or missing row is a failure.
package verify

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Manifest mirrors the file written by package seed.
type Manifest struct {
	Seed   int64           `json:"seed"`
	Rows   int             `json:"rows"`
	Tables []TableManifest `json:"tables"`
}

// TableManifest is the planted set for one table.
type TableManifest struct {
	Name    string    `json:"name"`
	Rows    int       `json:"rows"`
	Planted []Planted `json:"planted"`
}

// Planted is one expected difference.
type Planted struct {
	PK     string `json:"pk"`
	Kind   string `json:"kind"`
	Column string `json:"column,omitempty"`
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`
}

// Result is one observed difference, normalised for comparison.
type Result struct {
	Table   string
	PK      string
	Kind    string
	Columns []string
}

// Report is the outcome of checking observed results against the manifest.
type Report struct {
	Expected     int      `json:"expected"`
	Observed     int      `json:"observed"`
	Missing      []string `json:"missing"`       // planted but not reported
	Unexpected   []string `json:"unexpected"`    // reported but not planted
	KindMismatch []string `json:"kind_mismatch"` // same row, different kind
	ColMismatch  []string `json:"col_mismatch"`  // modified rows, different column set
	OK           bool     `json:"ok"`
}

// LoadManifest reads a manifest file.
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// Check compares observed results with the manifest.
func Check(m *Manifest, observed []Result) *Report {
	exp := keyMap(m)
	got := map[string][]string{}
	for _, r := range observed {
		k := key(r.Table, r.PK)
		got[k] = append(got[k], r.Kind)
	}
	rep := &Report{}
	for k, planted := range exp {
		rep.Expected += len(planted)
		kinds, ok := got[k]
		if !ok {
			for range planted {
				rep.Missing = append(rep.Missing, k)
			}
			continue
		}
		if len(kinds) == 0 {
			continue
		}
		matched := false
		for _, kind := range kinds {
			if kind == planted[0].Kind {
				matched = true
				break
			}
		}
		if !matched {
			rep.KindMismatch = append(rep.KindMismatch, fmt.Sprintf("%s: want %s got %v", k, planted[0].Kind, kinds))
		}
	}
	seen := map[string]bool{}
	for k, kinds := range got {
		seen[k] = true
		_ = kinds
	}
	for k := range seen {
		if _, ok := exp[k]; !ok {
			rep.Unexpected = append(rep.Unexpected, k)
		}
	}
	rep.Observed = len(observed)
	sort.Strings(rep.Missing)
	sort.Strings(rep.Unexpected)
	sort.Strings(rep.KindMismatch)
	rep.OK = len(rep.Missing) == 0 && len(rep.Unexpected) == 0 && len(rep.KindMismatch) == 0 && len(rep.ColMismatch) == 0
	return rep
}

func key(table, pk string) string { return table + "\x00" + pk }

func keyMap(m *Manifest) map[string][]Planted {
	out := map[string][]Planted{}
	for _, t := range m.Tables {
		for _, p := range t.Planted {
			k := key(t.Name, p.PK)
			out[k] = append(out[k], p)
		}
	}
	return out
}

// String renders the report for a terminal.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "expected %d difference(s), observed %d\n", r.Expected, r.Observed)
	if r.OK {
		b.WriteString("MATCH: the comparison reproduced the planted difference list exactly\n")
		return b.String()
	}
	dump := func(label string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s (%d):\n", label, len(items))
		for i, s := range items {
			if i >= 40 {
				fmt.Fprintf(&b, "  ... and %d more\n", len(items)-i)
				break
			}
			fmt.Fprintf(&b, "  %s\n", s)
		}
	}
	dump("planted but NOT reported", r.Missing)
	dump("reported but NOT planted", r.Unexpected)
	dump("kind mismatch", r.KindMismatch)
	dump("column mismatch", r.ColMismatch)
	return b.String()
}
