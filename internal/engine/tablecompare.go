package engine

import (
	"context"
	"fmt"

	"bankcmp/internal/model"
	"bankcmp/internal/plan"
)

// CompareSpec resolves and compares one table spec, returning the result.
func (r *Runner) CompareSpec(ctx context.Context, spec model.TableSpec) (*TableResult, error) {
	ss, err := r.Src.Introspect(ctx, spec.Source())
	if err != nil {
		return nil, fmt.Errorf("introspect source %q: %w", spec.Source(), err)
	}
	ts, err := r.Tgt.Introspect(ctx, spec.Target())
	if err != nil {
		return nil, fmt.Errorf("introspect target %q: %w", spec.Target(), err)
	}
	tp, err := plan.Build(spec, r.Src, r.Tgt, ss, ts, model.Options{
		ChunkRows: r.Opts.ChunkRows, DiffMaxRows: r.Opts.DiffMaxRows,
	})
	if err != nil {
		return nil, err
	}
	return r.RunTable(ctx, tp)
}
