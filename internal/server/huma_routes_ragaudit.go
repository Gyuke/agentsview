package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	_ "github.com/mattn/go-sqlite3"
)

// 展示 RAG 审计日志（rag-demo 的 audit/rag-audit.jsonl）。
// 读取接口不写库、不改 session 表；前端按 session_id 把审计事件对齐到
// agentsview 已摄取的 OpenFang 会话时间线。唯一的写接口是删除某个会话的
// 审计行（humaDeleteRagAuditSession），它只重写 rag-audit.jsonl 本身。

func (s *Server) registerRagAuditRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "RAG Audit")
	s.get(group, "/rag-audit", "List RAG audit events", s.humaListRagAudit)
	s.get(group, "/rag-audit/sessions", "List sessions that have RAG audit events", s.humaListRagAuditSessions)
	s.get(group, "/rag-audit/docs", "Aggregate per-document RAG usage stats", s.humaListRagAuditDocs)
	s.get(group, "/rag-audit/feedback", "List Feishu answer feedback", s.humaListRagAuditFeedback)
	s.deleteRoute(group, "/rag-audit/session", "Delete a session's RAG audit events", s.humaDeleteRagAuditSession)
	s.deleteRoute(group, "/rag-audit/trace", "Delete one trace and its evaluation data", s.humaDeleteRagAuditTrace)
	s.raw(group, http.MethodGet, "/rag-audit/benchmark-export", "Export reviewed rounds for the benchmark harness", "application/yaml", s.humaExportRagBenchmark)
	s.get(group, "/rag-audit/benchmark-verdict", "List human verdicts for reviewed rounds", s.humaListRagBenchmarkVerdicts)
	s.put(group, "/rag-audit/benchmark-verdict", "Record a human verdict for a reviewed round", s.humaSetRagBenchmarkVerdict)
	s.deleteRoute(group, "/rag-audit/benchmark-verdict", "Remove a human verdict", s.humaDeleteRagBenchmarkVerdict)
	// 归因报告：消费 harness 的输出（reports/*.json），并提供后台生成入口。
	s.get(group, "/rag-audit/benchmark-reports", "List generated attribution reports", s.humaListRagBenchmarkReports)
	s.raw(group, http.MethodGet, "/rag-audit/benchmark-report", "Read one attribution report", "application/json", s.humaGetRagBenchmarkReport)
	s.get(group, "/rag-audit/benchmark-run", "Attribution run status", s.humaGetRagBenchmarkRun)
	s.post(group, "/rag-audit/benchmark-run", "Start an attribution run in background", s.humaStartRagBenchmarkRun)
	s.get(group, "/rag-audit/benchmark-check", "Check attribution prerequisites", s.humaCheckRagBenchmark)
}

// ---- 输入 ----

type ragAuditInput struct {
	SessionID string `query:"session_id" doc:"Filter by OpenFang session uuid (audit session_id)"`
	KBName    string `query:"kb_name" doc:"Filter by knowledge base name"`
	Operation string `query:"operation" doc:"search | feedback | all (default all)"`
	Origin    string `query:"origin" doc:"mcp | cli | eval | all (default all)"`
	Limit     int    `query:"limit" minimum:"0" maximum:"1000" doc:"Max events to return (default 100)"`
}

func (in *ragAuditInput) effective() (operation, origin string, limit int) {
	operation = in.Operation
	if operation == "" {
		operation = "all"
	}
	origin = in.Origin
	if origin == "" {
		origin = "all"
	}
	limit = in.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	return operation, origin, limit
}

// ---- 解析层：宽松结构，兼容 rag.audit.v1/v2 与后续 v3(feedback) ----

type ragAuditLine struct {
	Timestamp string          `json:"timestamp"`
	Origin    string          `json:"origin"`
	Tool      string          `json:"tool"`
	SessionID string          `json:"session_id"`
	TraceID   string          `json:"trace_id"`
	Operation string          `json:"operation"`
	Request   ragAuditRequest `json:"request"`
	Response  ragAuditResp    `json:"response"`
}

type ragAuditRequest struct {
	KBName         string   `json:"kb_name"`
	Query          string   `json:"query"`
	TopK           *int     `json:"top_k"`
	ScoreThreshold *float64 `json:"score_threshold"`
	Strategy       string   `json:"strategy"`
	RerankEnabled  *bool    `json:"rerank_enabled"`
	// feedback 事件字段
	UsedChunkIDs []string `json:"used_chunk_ids"`
	Adopted      *bool    `json:"adopted"`
	Note         string   `json:"note"`
}

type ragAuditResp struct {
	DurationMs    *uint64               `json:"duration_ms"`
	HitCount      *int                  `json:"hit_count"`
	Hits          []ragAuditResponseHit `json:"hits"`
	CandidatePool []string              `json:"candidate_pool"`
}

type ragAuditResponseHit struct {
	Score float64           `json:"score"`
	Chunk ragAuditRespChunk `json:"chunk"`
}

type ragAuditRespChunk struct {
	DocPath string `json:"doc_path"`
	ChunkID string `json:"chunk_id"`
}

// ---- 输出层：摘要 hit（不带完整 text，控制体积） ----

type ragAuditEvent struct {
	Timestamp      string        `json:"timestamp"`
	SessionID      string        `json:"session_id"`
	TraceID        string        `json:"trace_id,omitempty"`
	Origin         string        `json:"origin,omitempty"`
	Operation      string        `json:"operation,omitempty"`
	Tool           string        `json:"tool"`
	KBName         string        `json:"kb_name"`
	Query          string        `json:"query"`
	TopK           *int          `json:"top_k,omitempty"`
	ScoreThreshold *float64      `json:"score_threshold,omitempty"`
	Strategy       string        `json:"strategy,omitempty"`
	RerankEnabled  *bool         `json:"rerank_enabled,omitempty"`
	DurationMs     *uint64       `json:"duration_ms,omitempty"`
	HitCount       *int          `json:"hit_count,omitempty"`
	Hits           []ragAuditHit `json:"hits,omitempty"`
	// feedback 事件字段
	UsedChunkIDs []string `json:"used_chunk_ids,omitempty"`
	Adopted      *bool    `json:"adopted,omitempty"`
	Note         string   `json:"note,omitempty"`
}

type ragAuditHit struct {
	DocPath string  `json:"doc_path"`
	ChunkID string  `json:"chunk_id"`
	Score   float64 `json:"score"`
}

type ragAuditEvents struct {
	Events   []ragAuditEvent `json:"events"`
	Total    int             `json:"total"`
	BadLines int             `json:"bad_lines"`
}

type ragAuditSession struct {
	SessionID  string `json:"session_id"`
	EventCount int    `json:"event_count"`
	FirstTs    string `json:"first_ts"`
	LastTs     string `json:"last_ts"`
}

type ragAuditSessionList struct {
	Sessions []ragAuditSession `json:"sessions"`
	Total    int               `json:"total"`
	BadLines int               `json:"bad_lines"`
}

// ---- 文档使用频率聚合（镜像 rag-demo doc-stats 的口径） ----

type ragAuditDocsInput struct {
	Origin string `query:"origin" doc:"mcp | cli | eval | all (default mcp, 与 doc-stats 一致排除 eval/demo 噪音)"`
	KBName string `query:"kb_name" doc:"Filter by knowledge base name"`
	Top    int    `query:"top" minimum:"0" maximum:"1000" doc:"Max docs to return (default 50)"`
}

func (in *ragAuditDocsInput) effective() (origin string, top int) {
	origin = in.Origin
	if origin == "" {
		origin = "mcp"
	}
	top = in.Top
	if top <= 0 {
		top = 50
	}
	if top > 1000 {
		top = 1000
	}
	return origin, top
}

// ragAuditDocStat 单篇文档的使用统计。文档唯一键为 (kb_name, doc_path)。
type ragAuditDocStat struct {
	KBName              string   `json:"kb_name"`
	DocPath             string   `json:"doc_path"`
	TriggerCount        int      `json:"trigger_count"`
	RetrievedChunkCount int      `json:"retrieved_chunk_count"`
	ConsumedChunkCount  int      `json:"consumed_chunk_count"`
	ConsumedRatio       float64  `json:"consumed_ratio"`
	LastTriggered       string   `json:"last_triggered,omitempty"`
	ExampleQueries      []string `json:"example_queries,omitempty"`
}

type ragAuditDocs struct {
	Docs                []ragAuditDocStat `json:"docs"`
	TotalEvents         int               `json:"total_events"`
	MatchedSearchEvents int               `json:"matched_search_events"`
	DistinctDocs        int               `json:"distinct_docs"`
	TriggeredDocs       int               `json:"triggered_docs"`
	BadLines            int               `json:"bad_lines"`
}

// readRagAuditLines 逐行解析审计文件，跳过坏行并统计数量。
func readRagAuditLines(path string) ([]ragAuditLine, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var lines []ragAuditLine
	bad := 0
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var line ragAuditLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			bad++
			continue
		}
		lines = append(lines, line)
	}
	return lines, bad, nil
}

func (s *Server) humaListRagAudit(
	_ context.Context,
	in *ragAuditInput,
) (*jsonOutput[*ragAuditEvents], error) {
	lines, bad, err := readRagAuditLines(s.cfg.RAGAuditPath)
	if err != nil {
		return nil, apiError(http.StatusNotFound,
			fmt.Sprintf("cannot read RAG audit file: %v", err))
	}
	operation, origin, limit := in.effective()

	var events []ragAuditEvent
	for _, line := range lines {
		if in.SessionID != "" && line.SessionID != in.SessionID {
			continue
		}
		if in.KBName != "" && line.Request.KBName != in.KBName {
			continue
		}
		if operation != "all" && line.Operation != operation {
			continue
		}
		if origin != "all" && line.Origin != origin {
			continue
		}
		event := ragAuditEvent{
			Timestamp:      line.Timestamp,
			SessionID:      line.SessionID,
			TraceID:        line.TraceID,
			Origin:         line.Origin,
			Operation:      line.Operation,
			Tool:           line.Tool,
			KBName:         line.Request.KBName,
			Query:          line.Request.Query,
			TopK:           line.Request.TopK,
			ScoreThreshold: line.Request.ScoreThreshold,
			Strategy:       line.Request.Strategy,
			RerankEnabled:  line.Request.RerankEnabled,
			DurationMs:     line.Response.DurationMs,
			HitCount:       line.Response.HitCount,
			UsedChunkIDs:   line.Request.UsedChunkIDs,
			Adopted:        line.Request.Adopted,
			Note:           line.Request.Note,
		}
		for _, hit := range line.Response.Hits {
			event.Hits = append(event.Hits, ragAuditHit{
				DocPath: hit.Chunk.DocPath,
				ChunkID: hit.Chunk.ChunkID,
				Score:   hit.Score,
			})
		}
		events = append(events, event)
	}

	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Timestamp < events[j].Timestamp
	})
	total := len(events)
	if total > limit {
		events = events[:limit]
	}
	return &jsonOutput[*ragAuditEvents]{
		Body: &ragAuditEvents{Events: events, Total: total, BadLines: bad},
	}, nil
}

func (s *Server) humaListRagAuditSessions(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[*ragAuditSessionList], error) {
	lines, bad, err := readRagAuditLines(s.cfg.RAGAuditPath)
	if err != nil {
		return nil, apiError(http.StatusNotFound,
			fmt.Sprintf("cannot read RAG audit file: %v", err))
	}

	type agg struct {
		count int
		first string
		last  string
	}
	byID := map[string]*agg{}
	for _, line := range lines {
		if line.SessionID == "" {
			continue
		}
		entry, ok := byID[line.SessionID]
		if !ok {
			entry = &agg{}
			byID[line.SessionID] = entry
		}
		entry.count++
		if entry.first == "" || line.Timestamp < entry.first {
			entry.first = line.Timestamp
		}
		if line.Timestamp > entry.last {
			entry.last = line.Timestamp
		}
	}

	var sessions []ragAuditSession
	for id, entry := range byID {
		sessions = append(sessions, ragAuditSession{
			SessionID:  id,
			EventCount: entry.count,
			FirstTs:    entry.first,
			LastTs:     entry.last,
		})
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].FirstTs < sessions[j].FirstTs
	})

	return &jsonOutput[*ragAuditSessionList]{
		Body: &ragAuditSessionList{Sessions: sessions, Total: len(sessions), BadLines: bad},
	}, nil
}

// humaListRagAuditDocs 聚合全量审计为"每篇文档的使用统计"，供前端"文档频率"页签展示。
// 口径与 rag-demo audit_stats::doc_stats 一致：
//   - trigger_count：命中该文档的 search 事件数（同事件同文档多 chunk 算 1 次）；
//   - retrieved_chunk_count：检索命中的去重 chunk_id 数；
//   - consumed：出现在 adopted != false 的 feedback used_chunk_ids 里的去重 chunk_id 数。
func (s *Server) humaListRagAuditDocs(
	_ context.Context,
	in *ragAuditDocsInput,
) (*jsonOutput[*ragAuditDocs], error) {
	lines, bad, err := readRagAuditLines(s.cfg.RAGAuditPath)
	if err != nil {
		return nil, apiError(http.StatusNotFound,
			fmt.Sprintf("cannot read RAG audit file: %v", err))
	}
	origin, top := in.effective()
	matchOrigin := func(o string) bool {
		return origin == "all" || o == origin
	}

	// 1) 收集"被采用"的 chunk_id（feedback 且 adopted != false；adopted=false 视为主动放弃）。
	adopted := map[string]bool{}
	for _, line := range lines {
		if line.Operation != "feedback" {
			continue
		}
		if line.Request.Adopted != nil && !*line.Request.Adopted {
			continue
		}
		for _, cid := range line.Request.UsedChunkIDs {
			adopted[cid] = true
		}
	}

	// 2) 叠加 search 命中到各文档。
	type docAgg struct {
		kbName         string
		docPath        string
		trigger        int
		retrieved      map[string]bool
		consumed       map[string]bool
		lastTriggered  string
		exampleQueries []string
		querySeen      map[string]bool
	}
	docs := map[string]*docAgg{}
	key := func(kb, dp string) string { return kb + "\x00" + dp }

	matched := 0
	for _, line := range lines {
		if line.Operation != "search" || !matchOrigin(line.Origin) {
			continue
		}
		if in.KBName != "" && line.Request.KBName != in.KBName {
			continue
		}
		matched++
		// 同事件内同一文档多 chunk 只算 1 次触发。
		eventSeen := map[string]bool{}
		for _, hit := range line.Response.Hits {
			dp := hit.Chunk.DocPath
			if dp == "" {
				continue
			}
			k := key(line.Request.KBName, dp)
			agg, ok := docs[k]
			if !ok {
				agg = &docAgg{
					kbName:    line.Request.KBName,
					docPath:   dp,
					retrieved: map[string]bool{},
					consumed:  map[string]bool{},
					querySeen: map[string]bool{},
				}
				docs[k] = agg
			}
			if cid := hit.Chunk.ChunkID; cid != "" {
				agg.retrieved[cid] = true
				if adopted[cid] {
					agg.consumed[cid] = true
				}
			}
			if !eventSeen[k] {
				eventSeen[k] = true
				agg.trigger++
			}
			if line.Timestamp > agg.lastTriggered {
				agg.lastTriggered = line.Timestamp
			}
			if q := line.Request.Query; q != "" && !agg.querySeen[q] && len(agg.exampleQueries) < 3 {
				agg.querySeen[q] = true
				agg.exampleQueries = append(agg.exampleQueries, q)
			}
		}
	}

	// 3) 汇总、排序、截断。
	var list []ragAuditDocStat
	for _, agg := range docs {
		retrieved := len(agg.retrieved)
		consumed := len(agg.consumed)
		ratio := 0.0
		if retrieved > 0 {
			ratio = float64(consumed) / float64(retrieved)
		}
		list = append(list, ragAuditDocStat{
			KBName:              agg.kbName,
			DocPath:             agg.docPath,
			TriggerCount:        agg.trigger,
			RetrievedChunkCount: retrieved,
			ConsumedChunkCount:  consumed,
			ConsumedRatio:       ratio,
			LastTriggered:       agg.lastTriggered,
			ExampleQueries:      agg.exampleQueries,
		})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].TriggerCount != list[j].TriggerCount {
			return list[i].TriggerCount > list[j].TriggerCount
		}
		if list[i].KBName != list[j].KBName {
			return list[i].KBName < list[j].KBName
		}
		return list[i].DocPath < list[j].DocPath
	})
	triggered := 0
	for _, d := range list {
		if d.TriggerCount > 0 {
			triggered++
		}
	}
	if len(list) > top {
		list = list[:top]
	}

	return &jsonOutput[*ragAuditDocs]{
		Body: &ragAuditDocs{
			Docs:                list,
			TotalEvents:         len(lines),
			MatchedSearchEvents: matched,
			DistinctDocs:        len(docs),
			TriggeredDocs:       triggered,
			BadLines:            bad,
		},
	}, nil
}

// ---- 飞书人工反馈（openfang.db 的 answer_feedback）----
//
// 与 RAG 审计一样是只读旁路：不写库、不改 session 表。读取 OpenFang 自己的
// answer_feedback + answer_feedback_targets，按 trace_id 关联，供前端在 trace
// 组头上展示"有帮助 / 回答错误 / 需改进"徽标及用户评语。

type ragAuditFeedbackInput struct {
	SessionID string `query:"session_id" doc:"Filter by OpenFang session uuid (audit session_id)"`
}

type ragAuditFeedback struct {
	TraceID   string `json:"trace_id"`
	SessionID string `json:"session_id"`
	Reviewer  string `json:"reviewer"`
	Rating    string `json:"rating"`
	Reason    string `json:"reason,omitempty"`
	Comment   string `json:"comment,omitempty"`
	// CreatedAt is the review time; the UI shows it to explain disagreement.
	CreatedAt string `json:"created_at"`
}

type ragAuditFeedbackList struct {
	Feedback []ragAuditFeedback `json:"feedback"`
	Total    int                `json:"total"`
}

func (s *Server) humaListRagAuditFeedback(
	_ context.Context,
	in *ragAuditFeedbackInput,
) (*jsonOutput[*ragAuditFeedbackList], error) {
	if s.cfg.OpenFangDBPath == "" {
		// 未配置 openfang.db 时静默返回空，不阻塞 RAG 审计页面其余部分。
		return &jsonOutput[*ragAuditFeedbackList]{
			Body: &ragAuditFeedbackList{},
		}, nil
	}

	db, err := sql.Open("sqlite3", "file:"+s.cfg.OpenFangDBPath+"?mode=ro")
	if err != nil {
		return nil, apiError(http.StatusNotFound,
			fmt.Sprintf("cannot open OpenFang DB: %v", err))
	}
	defer db.Close()

	query := `SELECT t.trace_id, t.session_id, f.reviewer_open_id, f.rating, f.reason, f.comment, f.created_at
		FROM answer_feedback f
		JOIN answer_feedback_targets t ON t.answer_id = f.answer_id
		WHERE t.channel = 'feishu' AND f.rating IS NOT NULL`
	args := []any{}
	if in.SessionID != "" {
		query += " AND t.session_id = ?"
		args = append(args, in.SessionID)
	}
	query += " ORDER BY f.created_at"

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, internalError("query OpenFang answer feedback", err)
	}
	defer rows.Close()

	var feedback []ragAuditFeedback
	for rows.Next() {
		var traceID, sessionID, reviewer, rating string
		var reason, comment, createdAt sql.NullString
		if err := rows.Scan(
			&traceID, &sessionID, &reviewer, &rating,
			&reason, &comment, &createdAt,
		); err != nil {
			return nil, internalError("scan OpenFang answer feedback", err)
		}
		if traceID == "" {
			continue
		}
		feedback = append(feedback, ragAuditFeedback{
			TraceID:   traceID,
			SessionID: sessionID,
			Reviewer:  reviewer,
			Rating:    rating,
			Reason:    reason.String,
			Comment:   comment.String,
			CreatedAt: createdAt.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, internalError("iterate OpenFang answer feedback", err)
	}

	return &jsonOutput[*ragAuditFeedbackList]{
		Body: &ragAuditFeedbackList{Feedback: feedback, Total: len(feedback)},
	}, nil
}

// ---- 删除某个会话的审计行 ----
//
// 调用前必须先停掉 rag-demo。rag-demo 启动时以 append 模式打开 rag-audit.jsonl
// 并长期持有该 fd；若在它运行期间重写文件，它的后续写入会落到被替换掉的旧 inode
// 上而静默丢失（rename 换文件时），或与本次重写交错产生坏行（原地截断时）。
// 这里不做自动检测——是否已停由使用者保证，UI 会明确提示。
//
// 为重写采用原地截断（O_TRUNC）而非 rename：即便使用者误在 rag-demo 运行时调用，
// 也只是内容竞争，不会把文件名与进程持有的 inode 解耦、造成"看着正常但写入丢失"
// 的更隐蔽后果。

type ragAuditDeleteInput struct {
	SessionID string `query:"session_id" doc:"Session uuid whose audit events should be deleted"`
}

type ragAuditDeleteResult struct {
	DeletedLines   int    `json:"deleted_lines"`
	RemainingLines int    `json:"remaining_lines"`
	BackupPath     string `json:"backup_path,omitempty"`
}

func (s *Server) humaDeleteRagAuditSession(
	_ context.Context,
	in *ragAuditDeleteInput,
) (*jsonOutput[*ragAuditDeleteResult], error) {
	sessionID := strings.TrimSpace(in.SessionID)
	if sessionID == "" {
		return nil, apiError(http.StatusBadRequest, "session_id is required")
	}
	path := s.cfg.RAGAuditPath
	if path == "" {
		return nil, apiError(http.StatusNotFound, "RAG audit path is not configured")
	}

	// 读全量原文（保留未命中行的原始字节），再按 session_id 过滤。
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
			SessionID string `json:"session_id"`
		}
		// 解析失败的行原样保留（与读取路径"跳过坏行"的宽松策略一致，不误删）。
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			kept = append(kept, line)
			continue
		}
		if probe.SessionID == sessionID {
			deleted++
			continue
		}
		kept = append(kept, line)
	}
	if deleted == 0 {
		return &jsonOutput[*ragAuditDeleteResult]{
			Body: &ragAuditDeleteResult{RemainingLines: len(lines) - 1},
		}, nil
	}

	// 备份：沿用磁盘上已有的 _bak_<date> 命名约定，便于误删恢复。
	backupPath := fmt.Sprintf("%s_bak_%s", path, time.Now().Format("20060102"))
	if err := os.WriteFile(backupPath, raw, 0o644); err != nil {
		return nil, internalError("backup RAG audit file", err)
	}

	// 原地截断重写：保留原 inode 与文件权限，避免改名换 inode 带来的写入丢失。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return nil, internalError("open RAG audit file for rewrite", err)
	}
	if _, err := f.WriteString(strings.Join(kept, "\n")); err != nil {
		f.Close()
		return nil, internalError("rewrite RAG audit file", err)
	}
	if err := f.Close(); err != nil {
		return nil, internalError("close RAG audit file", err)
	}

	return &jsonOutput[*ragAuditDeleteResult]{
		Body: &ragAuditDeleteResult{
			DeletedLines:   deleted,
			RemainingLines: len(kept) - 1,
			BackupPath:     backupPath,
		},
	}, nil
}
