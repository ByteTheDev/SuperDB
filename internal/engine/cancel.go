package engine

import "context"

// rowTicker amortizes context cancellation checks inside hot scan loops.
// The first call always checks so single-pass queries still honor an
// already-expired context; subsequent calls check every 64 rows.
type rowTicker struct{ n uint32 }

func (t *rowTicker) tick(ctx context.Context) error {
	t.n++
	if t.n != 1 && t.n&63 != 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
