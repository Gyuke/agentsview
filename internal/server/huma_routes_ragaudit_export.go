package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	yaml "go.yaml.in/yaml/v3"
)

const (
	ragBenchmarkSchemaVersion = 1

	finalAnswerFound             = "found"
	finalAnswerNoEligible        = "no_eligible_text_answer"
	finalAnswerMissingWindow     = "missing_window"
	finalAnswerNotRequested      = "not_requested"
	benchmarkAlignmentTimeWindow = "time_window"
)

// RAG benchmark export: one case per reviewed Feishu round, for the upstream
// failure-attribution harness.
//
// A case is only exported when it carries human feedback and can be tied to a
// Feishu question, because only those rounds participate in closed-loop
// evaluation. `final_answer` and `user_question` live only in openfang.db
// session messages, which this endpoint reads read-only.

type ragBenchmarkExportInput struct {
	Rating         string `query:"rating" doc:"helpful | incorrect | needs_improvement | all (default all)"`
	Session        string `query:"session" doc:"Limit to one session uuid"`
	Consensus      string `query:"consensus" doc:"single | agreed | split | all (default all)"`
	IncludeAnswer  bool   `query:"include_answer" default:"true" doc:"Include the agent's final answer (default true)"`
	AnswerMaxBytes int    `query:"answer_max_bytes" doc:"Truncate the final answer to this many bytes (default 4096)"`
}

const defaultAnswerMaxBytes = 4096

type ragBenchmarkRating struct {
	Reviewer  string `yaml:"reviewer"`
	Rating    string `yaml:"rating"`
	CreatedAt string `yaml:"created_at"`
}

type ragBenchmarkCase struct {
	TraceID           string               `yaml:"trace_id"`
	Session           string               `yaml:"session"`
	Ratings           []ragBenchmarkRating `yaml:"ratings"`
	Consensus         string               `yaml:"consensus"`
	EffectiveRating   *string              `yaml:"effective_rating"`
	Rating            *string              `yaml:"rating,omitempty"`
	Rounds            int                  `yaml:"rounds"`
	UserQuestion      *string              `yaml:"user_question"`
	FinalAnswer       *string              `yaml:"final_answer,omitempty"`
	FinalAnswerStatus string               `yaml:"final_answer_status"`
	Alignment         string               `yaml:"alignment"`
	AllQueries        []string             `yaml:"all_queries"`
	LastQuery         string               `yaml:"last_query,omitempty"`
	HitDocPaths       []string             `yaml:"hit_doc_paths"`
	CandidatePool     []string             `yaml:"candidate_pool,omitempty"`
}

type ragBenchmarkSourceFingerprint struct {
	FeedbackCount int    `yaml:"feedback_count"`
	AuditCount    int    `yaml:"audit_count"`
	QueriesSHA256 string `yaml:"queries_sha256"`
}

type ragBenchmarkDoc struct {
	SchemaVersion     int                           `yaml:"schema_version"`
	ExportedAt        string                        `yaml:"exported_at"`
	Source            string                        `yaml:"source"`
	SourceFingerprint ragBenchmarkSourceFingerprint `yaml:"source_fingerprint"`
	Queries           []ragBenchmarkCase            `yaml:"queries"`
}

// humaExportRagBenchmark renders the reviewed rounds as YAML.
func (s *Server) humaExportRagBenchmark(
	ctx context.Context,
	in *ragBenchmarkExportInput,
) (*bytesOutput, error) {
	lines, _, err := readRagAuditLines(s.cfg.RAGAuditPath)
	if err != nil {
		return nil, apiError(http.StatusNotFound,
			fmt.Sprintf("cannot read RAG audit file: %v", err))
	}

	verdicts, err := s.ragBenchmarkVerdicts(ctx)
	if err != nil {
		return nil, err
	}

	byTrace := indexAuditByTraceSession(lines)
	windows := auditTraceWindows(lines)
	feedback, err := s.readBenchmarkFeedback(in.Session, windows)
	if err != nil {
		return nil, err
	}
	feedbackCount := countBenchmarkFeedback(feedback)
	// 先读取带飞书反馈的轮次，再按需读取这些轮次对应的用户提问与最终回答。
	turns, answers, err := s.readBenchmarkSessionData(in.Session)
	if err != nil {
		return nil, err
	}
	answerMax := in.AnswerMaxBytes
	if answerMax <= 0 {
		answerMax = defaultAnswerMaxBytes
	}
	includeAnswer := in.IncludeAnswer

	cases := make([]ragBenchmarkCase, 0, len(feedback))
	for _, fb := range feedback {
		consensus, effective := consensusOf(fb.Ratings, verdicts[fb.TraceID])
		if !ratingMatches(in.Rating, effective) {
			continue
		}
		if in.Consensus != "" && in.Consensus != "all" && in.Consensus != consensus {
			continue
		}

		c := ragBenchmarkCase{
			TraceID:         fb.TraceID,
			Session:         fb.Session,
			Ratings:         toExportRatings(fb.Ratings),
			Consensus:       consensus,
			EffectiveRating: effective,
			Rating:          effective,
			UserQuestion:    firstNonEmpty(fb.UserQuestion),
			Alignment:       benchmarkAlignmentTimeWindow,
		}
		if c.UserQuestion == nil {
			// A reviewed Feishu round without a [From: ou_...] user question
			// cannot be tied back to the original input, so it is not a
			// closed-loop evaluation case.
			continue
		}

		traceKey := benchmarkTraceKey{
			traceID:   fb.TraceID,
			sessionID: fb.Session,
		}
		if evs := byTrace[traceKey]; len(evs) > 0 {
			c.Rounds = countSearchEvents(evs)
			c.AllQueries = searchQueries(evs)
			if len(c.AllQueries) > 0 {
				c.LastQuery = c.AllQueries[len(c.AllQueries)-1]
			}
			c.HitDocPaths = hitDocPaths(evs)
			c.CandidatePool = candidatePool(evs)
		}

		if includeAnswer {
			next := nextQuestionAfter(
				turns[fb.Session], fb.TraceWindow,
			)
			ans, status := finalAnswerForWithStatus(
				answers, fb.Session, fb.TraceWindow, next, answerMax,
			)
			c.FinalAnswerStatus = status
			if ans != "" {
				c.FinalAnswer = &ans
			}
		} else {
			c.FinalAnswerStatus = finalAnswerNotRequested
		}
		cases = append(cases, c)
	}

	// Deterministic ordering: the export must be byte-stable across runs.
	sort.SliceStable(cases, func(i, j int) bool {
		if cases[i].Session != cases[j].Session {
			return cases[i].Session < cases[j].Session
		}
		return cases[i].TraceID < cases[j].TraceID
	})

	queriesSHA256, err := benchmarkQueriesSHA256(cases)
	if err != nil {
		return nil, internalError("hash benchmark export", err)
	}
	out, err := yaml.Marshal(ragBenchmarkDoc{
		SchemaVersion: ragBenchmarkSchemaVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		Source:        "agentsview",
		SourceFingerprint: ragBenchmarkSourceFingerprint{
			FeedbackCount: feedbackCount,
			AuditCount:    len(lines),
			QueriesSHA256: queriesSHA256,
		},
		Queries: cases,
	})
	if err != nil {
		return nil, internalError("marshal benchmark export", err)
	}
	return &bytesOutput{
		ContentType:        "application/yaml; charset=utf-8",
		ContentDisposition: `attachment; filename="rag-benchmark-queries.yaml"`,
		Body:               out,
	}, nil
}

func countBenchmarkFeedback(feedback []*benchmarkFeedback) int {
	count := 0
	for _, fb := range feedback {
		count += len(fb.Ratings)
	}
	return count
}

func benchmarkQueriesSHA256(cases []ragBenchmarkCase) (string, error) {
	raw, err := yaml.Marshal(cases)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

// ---- 反馈：按 trace 聚合 + 关联该轮提问 ----

type benchmarkRating struct {
	Reviewer  string
	Rating    string
	CreatedAt string
}

type benchmarkFeedback struct {
	TraceID      string
	Session      string
	Ratings      []benchmarkRating
	UserQuestion *string
	// TraceWindow 是该 trace 在审计里的时间窗，用于对齐 openfang 会话消息。
	TraceWindow timeRange
}

type timeRange struct {
	start time.Time
	end   time.Time
	ok    bool
}

type benchmarkTraceKey struct {
	traceID   string
	sessionID string
}

func ratingMatches(filter string, effective *string) bool {
	if filter == "" || filter == "all" {
		return true
	}
	return effective != nil && *effective == filter
}

// consensusOf classifies reviewer agreement. A human verdict, when present,
// always wins: it is the user's authoritative decision for a disputed case.
func consensusOf(
	ratings []benchmarkRating, verdict *benchmarkVerdict,
) (string, *string) {
	if verdict != nil {
		v := verdict.EffectiveRating
		if len(ratings) <= 1 {
			return "single", &v
		}
		if allSameRating(ratings) {
			return "agreed", &v
		}
		return "split", &v
	}
	if len(ratings) <= 1 {
		if len(ratings) == 1 {
			v := ratings[0].Rating
			return "single", &v
		}
		return "single", nil
	}
	if allSameRating(ratings) {
		v := ratings[0].Rating
		return "agreed", &v
	}
	// Disagreement is a real signal: never pick a side on the user's behalf.
	return "split", nil
}

func allSameRating(ratings []benchmarkRating) bool {
	for _, r := range ratings[1:] {
		if r.Rating != ratings[0].Rating {
			return false
		}
	}
	return true
}

func firstNonEmpty(s *string) *string {
	if s == nil {
		return nil
	}
	if strings.TrimSpace(*s) == "" {
		return nil
	}
	return s
}

// openBenchmarkDB opens the OpenFang database read-only. A missing path is not
// an error: feedback and answers simply come back empty.
func openBenchmarkDB(path string) (*sql.DB, error) {
	if path == "" {
		return nil, nil
	}
	return sql.Open("sqlite3", "file:"+path+"?mode=ro")
}

// ---- 审计事件：按 trace 分组，取查询与命中文档 ----

// indexAuditByTraceSession groups audit lines by trace_id and session_id.
func indexAuditByTraceSession(
	lines []ragAuditLine,
) map[benchmarkTraceKey][]ragAuditLine {
	out := map[benchmarkTraceKey][]ragAuditLine{}
	for _, line := range lines {
		if line.TraceID == "" || line.SessionID == "" {
			continue
		}
		key := benchmarkTraceKey{
			traceID:   line.TraceID,
			sessionID: line.SessionID,
		}
		out[key] = append(out[key], line)
	}
	return out
}

func countSearchEvents(evs []ragAuditLine) int {
	n := 0
	for _, ev := range evs {
		if ev.Operation == "search" {
			n++
		}
	}
	return n
}

// searchQueries lists the queries fired in a round in audit order. Duplicate
// queries are kept because they are distinct retrieval rounds and the upstream
// harness historically relied on the final query in that sequence.
func searchQueries(evs []ragAuditLine) []string {
	var out []string
	for _, ev := range evs {
		if ev.Operation != "search" {
			continue
		}
		q := strings.TrimSpace(ev.Request.Query)
		if q == "" {
			continue
		}
		out = append(out, q)
	}
	return out
}

// hitDocPaths lists the distinct documents the round's searches retrieved.
func hitDocPaths(evs []ragAuditLine) []string {
	var out []string
	seen := map[string]bool{}
	for _, ev := range evs {
		if ev.Operation != "search" {
			continue
		}
		for _, hit := range ev.Response.Hits {
			doc := hit.Chunk.DocPath
			if doc == "" || seen[doc] {
				continue
			}
			seen[doc] = true
			out = append(out, doc)
		}
	}
	return out
}

// candidatePool lists the distinct pre-rerank chunk IDs seen across the
// round's searches. Each event's pool is already ordered by the retriever;
// deduplication only removes repeats introduced by multi-query rounds.
func candidatePool(evs []ragAuditLine) []string {
	var out []string
	seen := map[string]bool{}
	for _, ev := range evs {
		if ev.Operation != "search" {
			continue
		}
		for _, chunkID := range ev.Response.CandidatePool {
			if chunkID == "" || seen[chunkID] {
				continue
			}
			seen[chunkID] = true
			out = append(out, chunkID)
		}
	}
	return out
}

// ---- 人工裁决：写 agentsview 归档，不动 openfang.db ----

type ragBenchmarkVerdictInput struct {
	Body ragBenchmarkVerdictBody
}

type ragBenchmarkVerdictBody struct {
	TraceID         string `json:"trace_id" doc:"Audit trace_id being adjudicated"`
	EffectiveRating string `json:"effective_rating" doc:"helpful | incorrect | needs_improvement"`
	Note            string `json:"note,omitempty" doc:"Optional note explaining the verdict"`
}

type ragBenchmarkVerdictResult struct {
	TraceID         string `json:"trace_id"`
	EffectiveRating string `json:"effective_rating"`
}

type ragBenchmarkVerdictItem struct {
	TraceID         string `json:"trace_id"`
	EffectiveRating string `json:"effective_rating"`
	Note            string `json:"note,omitempty"`
	UpdatedAt       string `json:"updated_at"`
}

type ragBenchmarkVerdictList struct {
	Verdicts []ragBenchmarkVerdictItem `json:"verdicts"`
	Total    int                       `json:"total"`
}

type ragBenchmarkVerdictListInput struct {
	TraceID string `query:"trace_id" doc:"Limit to one audit trace_id"`
}

// 允许的裁决取值，与 openfang.db 的 rating 词表保持一致。
var benchmarkRatings = map[string]bool{
	"helpful":           true,
	"incorrect":         true,
	"needs_improvement": true,
}

// humaListRagBenchmarkVerdicts returns the human adjudications as JSON. The
// export endpoint returns YAML for the harness; the UI reads this instead so
// it never has to parse YAML.
func (s *Server) humaListRagBenchmarkVerdicts(
	ctx context.Context,
	in *ragBenchmarkVerdictListInput,
) (*jsonOutput[*ragBenchmarkVerdictList], error) {
	if s.db == nil {
		return &jsonOutput[*ragBenchmarkVerdictList]{
			Body: &ragBenchmarkVerdictList{},
		}, nil
	}
	raw, err := s.db.ListBenchmarkVerdicts(ctx)
	if err != nil {
		return nil, internalError("list benchmark verdicts", err)
	}
	traceID := strings.TrimSpace(in.TraceID)
	items := make([]ragBenchmarkVerdictItem, 0, len(raw))
	for id, v := range raw {
		if traceID != "" && id != traceID {
			continue
		}
		items = append(items, ragBenchmarkVerdictItem{
			TraceID:         id,
			EffectiveRating: v.EffectiveRating,
			Note:            v.Note,
			UpdatedAt:       v.UpdatedAt,
		})
	}
	// Map iteration is unordered; keep the response byte-stable.
	sort.Slice(items, func(i, j int) bool {
		return items[i].TraceID < items[j].TraceID
	})
	return &jsonOutput[*ragBenchmarkVerdictList]{
		Body: &ragBenchmarkVerdictList{Verdicts: items, Total: len(items)},
	}, nil
}

// humaSetRagBenchmarkVerdict records the human's authoritative rating for a
// disputed round. The export prefers this over any derived consensus.
func (s *Server) humaSetRagBenchmarkVerdict(
	_ context.Context,
	in *ragBenchmarkVerdictInput,
) (*jsonOutput[*ragBenchmarkVerdictResult], error) {
	if s.db == nil {
		return nil, apiError(http.StatusServiceUnavailable,
			"archive store is not available")
	}
	traceID := strings.TrimSpace(in.Body.TraceID)
	if traceID == "" {
		return nil, apiError(http.StatusBadRequest, "trace_id is required")
	}
	rating := strings.TrimSpace(in.Body.EffectiveRating)
	if !benchmarkRatings[rating] {
		return nil, apiError(http.StatusBadRequest,
			"effective_rating must be one of helpful, incorrect, needs_improvement")
	}
	if err := s.db.SetBenchmarkVerdict(traceID, rating, in.Body.Note); err != nil {
		return nil, internalError("set benchmark verdict", err)
	}
	return &jsonOutput[*ragBenchmarkVerdictResult]{
		Body: &ragBenchmarkVerdictResult{
			TraceID:         traceID,
			EffectiveRating: rating,
		},
	}, nil
}

type ragBenchmarkVerdictDeleteInput struct {
	TraceID string `query:"trace_id" doc:"Audit trace_id whose verdict should be removed"`
}

// humaDeleteRagBenchmarkVerdict removes an adjudication, so the case reverts to
// its derived consensus.
func (s *Server) humaDeleteRagBenchmarkVerdict(
	_ context.Context,
	in *ragBenchmarkVerdictDeleteInput,
) (*jsonOutput[*ragBenchmarkVerdictResult], error) {
	if s.db == nil {
		return nil, apiError(http.StatusServiceUnavailable,
			"archive store is not available")
	}
	traceID := strings.TrimSpace(in.TraceID)
	if traceID == "" {
		return nil, apiError(http.StatusBadRequest, "trace_id is required")
	}
	if err := s.db.DeleteBenchmarkVerdict(traceID); err != nil {
		return nil, internalError("delete benchmark verdict", err)
	}
	return &jsonOutput[*ragBenchmarkVerdictResult]{
		Body: &ragBenchmarkVerdictResult{TraceID: traceID},
	}, nil
}
