package db

import (
	"context"
	"database/sql"
	"fmt"
)

// BenchmarkVerdict is the human's authoritative rating for one reviewed RAG
// round, keyed by that round's audit trace_id. It is recorded when reviewers
// disagree and someone adjudicates in the UI.
type BenchmarkVerdict struct {
	TraceID         string
	EffectiveRating string
	Note            string
	UpdatedAt       string
}

// SetBenchmarkVerdict records (or replaces) the adjudicated rating for a trace.
func (db *DB) SetBenchmarkVerdict(traceID, rating, note string) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(context.Background(), `
		INSERT INTO rag_benchmark_verdicts (trace_id, effective_rating, note, updated_at)
		VALUES (?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		ON CONFLICT(trace_id) DO UPDATE SET
			effective_rating = excluded.effective_rating,
			note             = excluded.note,
			updated_at       = excluded.updated_at`,
		traceID, rating, note)
	if err != nil {
		return fmt.Errorf("setting benchmark verdict for %s: %w", traceID, err)
	}
	return nil
}

// DeleteBenchmarkVerdict removes an adjudication, reverting the case to its
// derived consensus.
func (db *DB) DeleteBenchmarkVerdict(traceID string) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		context.Background(),
		"DELETE FROM rag_benchmark_verdicts WHERE trace_id = ?", traceID,
	)
	if err != nil {
		return fmt.Errorf("deleting benchmark verdict for %s: %w", traceID, err)
	}
	return nil
}

// ListBenchmarkVerdicts returns every recorded verdict, keyed by trace_id.
func (db *DB) ListBenchmarkVerdicts(
	ctx context.Context,
) (map[string]BenchmarkVerdict, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT trace_id, effective_rating, COALESCE(note, ''), updated_at
		FROM rag_benchmark_verdicts`)
	if err != nil {
		if err == sql.ErrNoRows {
			return map[string]BenchmarkVerdict{}, nil
		}
		return nil, fmt.Errorf("listing benchmark verdicts: %w", err)
	}
	defer rows.Close()

	out := map[string]BenchmarkVerdict{}
	for rows.Next() {
		var v BenchmarkVerdict
		if err := rows.Scan(
			&v.TraceID, &v.EffectiveRating, &v.Note, &v.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning benchmark verdict: %w", err)
		}
		out[v.TraceID] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating benchmark verdicts: %w", err)
	}
	return out, nil
}
