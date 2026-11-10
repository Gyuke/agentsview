package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// RAG trace deletion removes one user-question/agent-loop/answer round from all
// stores that can carry evaluation data. The JSONL rewrite remains in place so
// an accidental deletion can be recovered from the daily backup.

type ragAuditTraceDeleteInput struct {
	TraceID   string `query:"trace_id" required:"true" doc:"Trace id to delete"`
	SessionID string `query:"session_id" required:"true" doc:"Session uuid that owns the trace"`
}

type ragAuditTraceDeleteResult struct {
	DeletedAuditLines      int    `json:"deleted_audit_lines"`
	RemainingAuditLines    int    `json:"remaining_audit_lines"`
	DeletedFeedbackTargets int64  `json:"deleted_feedback_targets"`
	DeletedFeedbackRows    int64  `json:"deleted_feedback_rows"`
	DeletedVerdicts        int    `json:"deleted_verdicts"`
	BackupPath             string `json:"backup_path,omitempty"`
}

type ragAuditTraceDeletePlan struct {
	path      string
	raw       []byte
	kept      []string
	deleted   int
	remaining int
}

func (s *Server) humaDeleteRagAuditTrace(
	ctx context.Context,
	in *ragAuditTraceDeleteInput,
) (*jsonOutput[*ragAuditTraceDeleteResult], error) {
	traceID := strings.TrimSpace(in.TraceID)
	if traceID == "" {
		return nil, apiError(http.StatusBadRequest, "trace_id is required")
	}
	sessionID := strings.TrimSpace(in.SessionID)
	if sessionID == "" {
		return nil, apiError(http.StatusBadRequest, "session_id is required")
	}
	if strings.TrimSpace(s.cfg.OpenFangDBPath) == "" {
		return nil, apiError(http.StatusBadRequest, "openfang_db_path is not configured")
	}
	if strings.TrimSpace(s.cfg.RAGAuditPath) == "" {
		return nil, apiError(http.StatusBadRequest, "rag_audit_path is not configured")
	}

	auditPlan, err := buildRagAuditTraceDeletePlan(
		s.cfg.RAGAuditPath, traceID, sessionID,
	)
	if err != nil {
		return nil, err
	}

	targets, feedback, err := deleteOpenFangTraceFeedback(
		ctx, s.cfg.OpenFangDBPath, traceID, sessionID,
	)
	if err != nil {
		return nil, internalError("delete OpenFang trace feedback", err)
	}

	verdicts, err := s.deleteTraceBenchmarkVerdict(ctx, traceID)
	if err != nil {
		return nil, err
	}

	backupPath, err := auditPlan.apply()
	if err != nil {
		return nil, internalError("rewrite RAG audit file", err)
	}

	return &jsonOutput[*ragAuditTraceDeleteResult]{
		Body: &ragAuditTraceDeleteResult{
			DeletedAuditLines:      auditPlan.deleted,
			RemainingAuditLines:    auditPlan.remaining,
			DeletedFeedbackTargets: targets,
			DeletedFeedbackRows:    feedback,
			DeletedVerdicts:        verdicts,
			BackupPath:             backupPath,
		},
	}, nil
}

func buildRagAuditTraceDeletePlan(
	path, traceID, sessionID string,
) (*ragAuditTraceDeletePlan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, apiError(http.StatusNotFound,
			fmt.Sprintf("cannot read RAG audit file: %v", err))
	}
	lines := strings.Split(string(raw), "\n")
	kept := make([]string, 0, len(lines))
	deleted := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			kept = append(kept, line)
			continue
		}
		var probe struct {
			TraceID   string `json:"trace_id"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			kept = append(kept, line)
			continue
		}
		if probe.TraceID == traceID && probe.SessionID == sessionID {
			deleted++
			continue
		}
		kept = append(kept, line)
	}
	remaining := len(kept)
	if len(kept) > 0 && kept[len(kept)-1] == "" {
		remaining--
	}
	return &ragAuditTraceDeletePlan{
		path:      path,
		raw:       raw,
		kept:      kept,
		deleted:   deleted,
		remaining: remaining,
	}, nil
}

func (p *ragAuditTraceDeletePlan) apply() (string, error) {
	if p.deleted == 0 {
		return "", nil
	}
	backupPath := fmt.Sprintf("%s_bak_%s", p.path, time.Now().Format("20060102_150405"))
	if err := os.WriteFile(backupPath, p.raw, 0o644); err != nil {
		return "", fmt.Errorf("backup RAG audit file: %w", err)
	}
	f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return "", fmt.Errorf("open RAG audit file for rewrite: %w", err)
	}
	if _, err := f.WriteString(strings.Join(p.kept, "\n")); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("rewrite RAG audit file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("sync RAG audit file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close RAG audit file: %w", err)
	}
	return backupPath, nil
}

func deleteOpenFangTraceFeedback(
	ctx context.Context,
	path, traceID, sessionID string,
) (targets, feedback int64, err error) {
	db, err := sql.Open(
		"sqlite3",
		"file:"+path+"?mode=rw&_busy_timeout=5000",
	)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return 0, 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		DELETE FROM answer_feedback
		WHERE answer_id IN (
			SELECT answer_id
			FROM answer_feedback_targets
			WHERE trace_id = ? AND session_id = ?
		)`, traceID, sessionID)
	if err != nil {
		return 0, 0, err
	}
	feedback, err = result.RowsAffected()
	if err != nil {
		return 0, 0, err
	}

	result, err = tx.ExecContext(ctx, `
		DELETE FROM answer_feedback_targets
		WHERE trace_id = ? AND session_id = ?`, traceID, sessionID)
	if err != nil {
		return 0, 0, err
	}
	targets, err = result.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return targets, feedback, nil
}

func (s *Server) deleteTraceBenchmarkVerdict(
	ctx context.Context, traceID string,
) (int, error) {
	if s.db == nil {
		return 0, nil
	}
	verdicts, err := s.db.ListBenchmarkVerdicts(ctx)
	if err != nil {
		return 0, internalError("list benchmark verdicts", err)
	}
	if _, ok := verdicts[traceID]; !ok {
		return 0, nil
	}
	if err := s.db.DeleteBenchmarkVerdict(traceID); err != nil {
		return 0, internalError("delete benchmark verdict", err)
	}
	return 1, nil
}
