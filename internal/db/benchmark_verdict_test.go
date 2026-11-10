package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBenchmarkVerdictLifecycle(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	got, err := d.ListBenchmarkVerdicts(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)

	require.NoError(t, d.SetBenchmarkVerdict("trace-1", "incorrect", "first"))
	got, err = d.ListBenchmarkVerdicts(ctx)
	require.NoError(t, err)
	require.Contains(t, got, "trace-1")
	assert.Equal(t, "incorrect", got["trace-1"].EffectiveRating)
	assert.Equal(t, "first", got["trace-1"].Note)
	assert.NotEmpty(t, got["trace-1"].UpdatedAt)

	require.NoError(t, d.SetBenchmarkVerdict("trace-1", "helpful", "changed"))
	got, err = d.ListBenchmarkVerdicts(ctx)
	require.NoError(t, err)
	require.Contains(t, got, "trace-1")
	assert.Equal(t, "helpful", got["trace-1"].EffectiveRating)
	assert.Equal(t, "changed", got["trace-1"].Note)

	updatedAt, err := time.Parse(time.RFC3339Nano, got["trace-1"].UpdatedAt)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), updatedAt, time.Minute)

	require.NoError(t, d.DeleteBenchmarkVerdict("trace-1"))
	got, err = d.ListBenchmarkVerdicts(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCopySessionMetadataPreservesBenchmarkVerdicts(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	require.NoError(t, srcDB.SetBenchmarkVerdict(
		"trace-1", "incorrect", "needs review",
	))
	require.NoError(t, srcDB.Close())

	dstDB := testDBAtPath(t, filepath.Join(dir, "dst.db"), "dst")
	defer dstDB.Close()

	require.NoError(t, dstDB.CopySessionMetadataFrom(t.Context(), srcPath))
	got, err := dstDB.ListBenchmarkVerdicts(context.Background())
	require.NoError(t, err)
	require.Contains(t, got, "trace-1")
	assert.Equal(t, "incorrect", got["trace-1"].EffectiveRating)
	assert.Equal(t, "needs review", got["trace-1"].Note)
	assert.NotEmpty(t, got["trace-1"].UpdatedAt)

	require.NoError(t, dstDB.CopySessionMetadataFrom(t.Context(), srcPath))
	got, err = dstDB.ListBenchmarkVerdicts(context.Background())
	require.NoError(t, err)
	require.Contains(t, got, "trace-1")
	assert.Equal(t, "incorrect", got["trace-1"].EffectiveRating)
}
