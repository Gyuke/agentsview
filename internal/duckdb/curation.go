package duckdb

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) SetBenchmarkVerdict(traceID, rating, note string) error {
	return db.ErrReadOnly
}

func (s *Store) DeleteBenchmarkVerdict(traceID string) error {
	return db.ErrReadOnly
}

// ListBenchmarkVerdicts returns an empty view for the disposable read mirror.
func (s *Store) ListBenchmarkVerdicts(
	_ context.Context,
) (map[string]db.BenchmarkVerdict, error) {
	return map[string]db.BenchmarkVerdict{}, nil
}
