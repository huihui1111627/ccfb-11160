// Package engine implements the chunked checksum comparison: it splits a
// table into key ranges, compares cheap aggregates per range, subdivides only
// the ranges that disagree, and materialises exact row level differences at
// the leaves.
package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"bankcmp/internal/dialect"
	"bankcmp/internal/model"
	"bankcmp/internal/plan"
)

// Limiter paces database work so a production source is not saturated.  It
// supports two independent budgets, rows per second and queries per second,
// both optional, and is safe for concurrent use.
type Limiter struct {
	rowRate float64
	qpsRate float64
	mu      sync.Mutex
	rows    float64
	queries float64
	last    time.Time
}

// NewLimiter builds a limiter; a zero rate means unlimited.
func NewLimiter(rowsPerSec, queriesPerSec int) *Limiter {
	l := &Limiter{rowRate: float64(rowsPerSec), qpsRate: float64(queriesPerSec)}
	if l.rowRate > 0 || l.qpsRate > 0 {
		l.last = time.Now()
	}
	if l.rowRate > 0 {
		l.rows = l.rowRate
	}
	if l.qpsRate > 0 {
		l.queries = l.qpsRate
	}
	return l
}

// Wait blocks until budget for n rows and one query is available.
func (l *Limiter) Wait(ctx context.Context, rows int) error {
	if l == nil || (l.rowRate <= 0 && l.qpsRate <= 0) {
		return nil
	}
	return l.acquire(ctx, float64(rows), 1)
}

func (l *Limiter) acquire(ctx context.Context, rows, queries float64) error {
	if l.rowRate > 0 {
		rows = clamp(rows, 0, l.rowRate*2)
	}
	for {
		l.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(l.last).Seconds()
		l.last = now
		if l.rowRate > 0 {
			l.rows += elapsed * l.rowRate
			if l.rows > l.rowRate*2 {
				l.rows = l.rowRate * 2
			}
		}
		if l.qpsRate > 0 {
			l.queries += elapsed * l.qpsRate
			if l.queries > l.qpsRate*2 {
				l.queries = l.qpsRate * 2
			}
		}
		var wait time.Duration
		if l.rowRate > 0 && l.rows < rows {
			wait = time.Duration((rows - l.rows) / l.rowRate * float64(time.Second))
		}
		if l.qpsRate > 0 && l.queries < queries {
			w2 := time.Duration((queries - l.queries) / l.qpsRate * float64(time.Second))
			if w2 > wait {
				wait = w2
			}
		}
		if wait > 0 {
			l.mu.Unlock()
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
			continue
		}
		if l.rowRate > 0 {
			l.rows -= rows
		}
		if l.qpsRate > 0 {
			l.queries -= queries
		}
		l.mu.Unlock()
		return nil
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Progress is a snapshot emitted while a table is compared.
type Progress struct {
	ChunksTotal int
	ChunksDone  int
	ChunksDirty int
	Diffs       int64
	RowsScanned int64
	Phase       string
}

// DiffSink receives row level differences as they are confirmed.
type DiffSink interface {
	Add(d model.Diff) error
}

var errStopped = context.Canceled

// stopFlag is a cooperative cancellation flag shared by workers.
type stopFlag struct{ v atomic.Bool }

func (s *stopFlag) stop()         { s.v.Store(true) }
func (s *stopFlag) stopped() bool { return s.v.Load() }

func dialFor(tp *plan.TablePlan, side plan.Side) dialect.Dialect {
	if side == plan.SideSource {
		return tp.SourceDial
	}
	return tp.TargetDial
}
