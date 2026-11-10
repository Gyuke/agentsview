package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestRagAuditTraceDeleteRemovesAllEvaluationData(t *testing.T) {
	fangDB, fangPath := newOpenFangFixture(t)
	addFeedbackTarget(t, fangDB, "ans-delete", "sess-1", "tr-delete", "feishu")
	addFeedbackTarget(t, fangDB, "ans-keep", "sess-1", "tr-keep", "feishu")
	addFeedbackTarget(t, fangDB, "ans-other-session", "sess-2", "tr-delete", "feishu")
	addFeedback(t, fangDB, "ans-delete", "ou_reviewer", "incorrect", "2026-09-08T12:00:00Z")
	addFeedback(t, fangDB, "ans-keep", "ou_reviewer", "helpful", "2026-09-08T12:01:00Z")
	addFeedback(t, fangDB, "ans-other-session", "ou_reviewer", "incorrect", "2026-09-08T12:02:00Z")

	auditPath := writeAuditFixture(t,
		searchLine("tr-delete", "sess-1", "2026-09-08T10:00:00Z", "q-delete-1", "doc/a"),
		searchLine("tr-delete", "sess-1", "2026-09-08T10:00:01Z", "q-delete-2", "doc/b"),
		searchLine("tr-keep", "sess-1", "2026-09-08T10:01:00Z", "q-keep", "doc/c"),
		searchLine("tr-delete", "sess-2", "2026-09-08T10:02:00Z", "q-other-session", "doc/d"),
	)
	originalAudit, err := os.ReadFile(auditPath)
	require.NoError(t, err)

	store := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, store.SetBenchmarkVerdict("tr-delete", "incorrect", "bad run"))
	require.NoError(t, store.SetBenchmarkVerdict("tr-keep", "helpful", "good run"))

	s := &Server{
		cfg: config.Config{
			RAGAuditPath:   auditPath,
			OpenFangDBPath: fangPath,
		},
		db: store,
	}

	out, err := s.humaDeleteRagAuditTrace(context.Background(), &ragAuditTraceDeleteInput{
		TraceID:   "tr-delete",
		SessionID: "sess-1",
	})
	require.NoError(t, err)
	require.NotNil(t, out.Body)
	assert.Equal(t, 2, out.Body.DeletedAuditLines)
	assert.Equal(t, 2, out.Body.RemainingAuditLines)
	assert.Equal(t, int64(1), out.Body.DeletedFeedbackTargets)
	assert.Equal(t, int64(1), out.Body.DeletedFeedbackRows)
	assert.Equal(t, 1, out.Body.DeletedVerdicts)
	assert.NotEmpty(t, out.Body.BackupPath)

	backup, err := os.ReadFile(out.Body.BackupPath)
	require.NoError(t, err)
	assert.Equal(t, originalAudit, backup)

	remainingAudit, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	assert.Contains(t, string(remainingAudit), "tr-keep")
	assert.Contains(t, string(remainingAudit), `"trace_id":"tr-delete","operation"`)
	assert.NotContains(t, string(remainingAudit),
		`"session_id":"sess-1","trace_id":"tr-delete"`)

	var feedbackRows int
	require.NoError(t, fangDB.QueryRow(
		"SELECT COUNT(*) FROM answer_feedback",
	).Scan(&feedbackRows))
	assert.Equal(t, 2, feedbackRows)

	var targetRows int
	require.NoError(t, fangDB.QueryRow(
		"SELECT COUNT(*) FROM answer_feedback_targets",
	).Scan(&targetRows))
	assert.Equal(t, 2, targetRows)

	verdicts, err := store.ListBenchmarkVerdicts(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, verdicts, "tr-delete")
	assert.Contains(t, verdicts, "tr-keep")

	again, err := s.humaDeleteRagAuditTrace(context.Background(), &ragAuditTraceDeleteInput{
		TraceID:   "tr-delete",
		SessionID: "sess-1",
	})
	require.NoError(t, err)
	assert.Equal(t, 0, again.Body.DeletedAuditLines)
	assert.Equal(t, int64(0), again.Body.DeletedFeedbackTargets)
	assert.Empty(t, again.Body.BackupPath)
}

func TestRagAuditTraceDeletePreservesMalformedAuditLines(t *testing.T) {
	fangDB, fangPath := newOpenFangFixture(t)
	addFeedbackTarget(t, fangDB, "ans-delete", "sess-1", "tr-delete", "feishu")
	addFeedback(t, fangDB, "ans-delete", "ou_reviewer", "incorrect", "2026-09-08T12:00:00Z")

	auditPath := writeAuditFixture(t,
		searchLine("tr-delete", "sess-1", "2026-09-08T10:00:00Z", "q-delete", "doc/a"),
		"{bad json",
	)
	store := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "archive.db"))
	s := &Server{
		cfg: config.Config{
			RAGAuditPath:   auditPath,
			OpenFangDBPath: fangPath,
		},
		db: store,
	}

	_, err := s.humaDeleteRagAuditTrace(context.Background(), &ragAuditTraceDeleteInput{
		TraceID:   "tr-delete",
		SessionID: "sess-1",
	})
	require.NoError(t, err)

	raw, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(raw), "{bad json"))
	assert.NotContains(t, string(raw), "q-delete")
}

func TestRagAuditTraceDeleteRejectsMissingInputs(t *testing.T) {
	s := &Server{cfg: config.Config{
		RAGAuditPath:   "audit.jsonl",
		OpenFangDBPath: "openfang.db",
	}}
	tests := []struct {
		name string
		in   ragAuditTraceDeleteInput
	}{
		{name: "missing trace", in: ragAuditTraceDeleteInput{SessionID: "sess"}},
		{name: "missing session", in: ragAuditTraceDeleteInput{TraceID: "trace"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.humaDeleteRagAuditTrace(context.Background(), &tc.in)
			require.Error(t, err)
			var apiErr *apiResponseError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.Status)
		})
	}
}
