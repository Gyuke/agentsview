package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestHostedBenchmarkVerdictsAreNotSharedAcrossTenants(t *testing.T) {
	store := &HostedStore{}
	require.ErrorIs(t, store.SetBenchmarkVerdict("trace", "helpful", "note"), db.ErrReadOnly)
	require.ErrorIs(t, store.DeleteBenchmarkVerdict("trace"), db.ErrReadOnly)
	verdicts, err := store.ListBenchmarkVerdicts(t.Context())
	require.NoError(t, err)
	assert.Empty(t, verdicts)
}
