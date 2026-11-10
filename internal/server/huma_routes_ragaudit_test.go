package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/config"
)

// 写一个临时审计 jsonl：两行合法（不同 session_id）+ 一行坏行。
func writeRagAuditFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rag-audit.jsonl")
	content := `{"timestamp":"2026-09-02T03:28:41Z","origin":"mcp","tool":"rag_search","session_id":"sess-1","trace_id":"tr-1","operation":"search","request":{"kb_name":"kb","query":"q1","top_k":5,"strategy":"hybrid","rerank_enabled":true},"response":{"status":"ok","duration_ms":5,"hit_count":2,"hits":[{"score":1.5,"chunk":{"doc_path":"doc1","chunk_id":"c1"}},{"score":2.0,"chunk":{"doc_path":"doc2","chunk_id":"c2"}}]}}
not-json-line
{"timestamp":"2026-09-02T03:29:00Z","origin":"mcp","tool":"rag_search","session_id":"sess-2","trace_id":"tr-2","operation":"search","request":{"kb_name":"kb","query":"q2","top_k":5},"response":{"status":"ok","duration_ms":3,"hit_count":0,"hits":[]}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestRagAuditFiltersBySessionAndSkipsBadLines(t *testing.T) {
	s := &Server{cfg: config.Config{RAGAuditPath: writeRagAuditFixture(t)}}

	// 按 session_id 过滤：sess-1 命中 1 条、2 个 hit，坏行被跳过。
	out, err := s.humaListRagAudit(context.Background(), &ragAuditInput{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("humaListRagAudit: %v", err)
	}
	body := out.Body
	if body.Total != 1 {
		t.Fatalf("expected Total 1, got %d", body.Total)
	}
	if body.BadLines != 1 {
		t.Fatalf("expected BadLines 1, got %d", body.BadLines)
	}
	if len(body.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(body.Events))
	}
	ev := body.Events[0]
	if ev.SessionID != "sess-1" || ev.Query != "q1" || ev.KBName != "kb" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if len(ev.Hits) != 2 || ev.Hits[0].ChunkID != "c1" || ev.Hits[1].DocPath != "doc2" {
		t.Fatalf("unexpected hits: %+v", ev.Hits)
	}
	if ev.HitCount == nil || *ev.HitCount != 2 {
		t.Fatalf("expected hit_count 2, got %v", ev.HitCount)
	}
}

func TestRagAuditAllEventsAndZeroHit(t *testing.T) {
	s := &Server{cfg: config.Config{RAGAuditPath: writeRagAuditFixture(t)}}

	out, err := s.humaListRagAudit(context.Background(), &ragAuditInput{})
	if err != nil {
		t.Fatalf("humaListRagAudit: %v", err)
	}
	if out.Body.Total != 2 {
		t.Fatalf("expected Total 2, got %d", out.Body.Total)
	}
	// 时间升序：sess-1 在前、sess-2 在后。
	if out.Body.Events[0].SessionID != "sess-1" || out.Body.Events[1].SessionID != "sess-2" {
		t.Fatalf("unexpected order: %+v", out.Body.Events)
	}
	if out.Body.Events[1].HitCount == nil || *out.Body.Events[1].HitCount != 0 {
		t.Fatalf("expected zero-hit event hit_count 0, got %v", out.Body.Events[1].HitCount)
	}
}

// 文档频率聚合fixture：一条 search（命中两文档、doc1 两个 chunk）、
// 一条 feedback（采用 c1、主动放弃 c2）、一条 eval 来源 search、一行坏行。
func writeRagAuditDocsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rag-audit.jsonl")
	content := `{"timestamp":"2026-09-02T03:28:41Z","origin":"mcp","tool":"rag_search","session_id":"sess-1","trace_id":"tr-1","operation":"search","request":{"kb_name":"kb","query":"q1","top_k":5},"response":{"status":"ok","duration_ms":5,"hit_count":2,"hits":[{"score":1.5,"chunk":{"doc_path":"doc1","chunk_id":"c1"}},{"score":2.0,"chunk":{"doc_path":"doc1","chunk_id":"c2"}},{"score":3.0,"chunk":{"doc_path":"doc2","chunk_id":"c3"}}]}}
{"timestamp":"2026-09-02T03:29:00Z","origin":"mcp","tool":"rag_feedback","session_id":"sess-1","trace_id":"tr-1","operation":"feedback","request":{"kb_name":"kb","query":"q1","used_chunk_ids":["c1"],"adopted":true},"response":{}}
{"timestamp":"2026-09-02T03:29:01Z","origin":"mcp","tool":"rag_feedback","session_id":"sess-1","trace_id":"tr-1","operation":"feedback","request":{"kb_name":"kb","query":"q1","used_chunk_ids":["c2"],"adopted":false},"response":{}}
{"timestamp":"2026-09-02T03:30:00Z","origin":"eval","tool":"rag_search","session_id":"sess-2","trace_id":"tr-2","operation":"search","request":{"kb_name":"kb","query":"eval-q","top_k":5},"response":{"status":"ok","duration_ms":3,"hit_count":1,"hits":[{"score":1.0,"chunk":{"doc_path":"doc3","chunk_id":"c9"}}]}}
not-json-line
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestRagAuditDocsAggregates(t *testing.T) {
	s := &Server{cfg: config.Config{RAGAuditPath: writeRagAuditDocsFixture(t)}}

	out, err := s.humaListRagAuditDocs(context.Background(), &ragAuditDocsInput{})
	if err != nil {
		t.Fatalf("humaListRagAuditDocs: %v", err)
	}
	body := out.Body
	if body.TotalEvents != 4 {
		t.Fatalf("expected TotalEvents 4 (合法行数, 坏行不计), got %d", body.TotalEvents)
	}
	if body.BadLines != 1 {
		t.Fatalf("expected BadLines 1, got %d", body.BadLines)
	}
	// 仅 1 条 mcp search 参与统计（eval 被 origin 过滤）。
	if body.MatchedSearchEvents != 1 {
		t.Fatalf("expected MatchedSearchEvents 1, got %d", body.MatchedSearchEvents)
	}
	if body.DistinctDocs != 2 {
		t.Fatalf("expected DistinctDocs 2 (doc1/doc2), got %d", body.DistinctDocs)
	}
	if body.TriggeredDocs != 2 {
		t.Fatalf("expected TriggeredDocs 2, got %d", body.TriggeredDocs)
	}

	// doc1：同事件两个 chunk 只算 1 次触发；retrieved 2；c1 被采用、c2 被放弃 -> consumed 1。
	d1 := findDocStat(t, body.Docs, "doc1")
	if d1.TriggerCount != 1 || d1.RetrievedChunkCount != 2 || d1.ConsumedChunkCount != 1 {
		t.Fatalf("unexpected doc1: %+v", d1)
	}
	if d1.ConsumedRatio < 0.499 || d1.ConsumedRatio > 0.501 {
		t.Fatalf("expected doc1 ratio ~0.5, got %f", d1.ConsumedRatio)
	}
	if d1.LastTriggered != "2026-09-02T03:28:41Z" {
		t.Fatalf("unexpected doc1 last_triggered: %q", d1.LastTriggered)
	}

	// doc2：单 chunk，c3 未被采用。
	d2 := findDocStat(t, body.Docs, "doc2")
	if d2.TriggerCount != 1 || d2.RetrievedChunkCount != 1 || d2.ConsumedChunkCount != 0 {
		t.Fatalf("unexpected doc2: %+v", d2)
	}
	if d2.ConsumedRatio != 0 {
		t.Fatalf("expected doc2 ratio 0, got %f", d2.ConsumedRatio)
	}

	// 按触发次数降序：doc1/doc2 同为 1 次，kb 相同按 doc_path 升序。
	if body.Docs[0].DocPath != "doc1" || body.Docs[1].DocPath != "doc2" {
		t.Fatalf("unexpected docs order: %+v", body.Docs)
	}
}

func findDocStat(t *testing.T, docs []ragAuditDocStat, docPath string) ragAuditDocStat {
	t.Helper()
	for _, d := range docs {
		if d.DocPath == docPath {
			return d
		}
	}
	t.Fatalf("doc %q not found in %+v", docPath, docs)
	return ragAuditDocStat{}
}

// origin=eval 时只统计 eval 来源：doc3 应被纳入；mcp 的 doc1/doc2 被过滤。
func TestRagAuditDocsFiltersByOrigin(t *testing.T) {
	s := &Server{cfg: config.Config{RAGAuditPath: writeRagAuditDocsFixture(t)}}

	out, err := s.humaListRagAuditDocs(context.Background(), &ragAuditDocsInput{Origin: "eval"})
	if err != nil {
		t.Fatalf("humaListRagAuditDocs: %v", err)
	}
	if out.Body.MatchedSearchEvents != 1 {
		t.Fatalf("expected MatchedSearchEvents 1, got %d", out.Body.MatchedSearchEvents)
	}
	d3 := findDocStat(t, out.Body.Docs, "doc3")
	if d3.TriggerCount != 1 {
		t.Fatalf("unexpected doc3: %+v", d3)
	}
}

func TestRagAuditSessionsAggregates(t *testing.T) {
	s := &Server{cfg: config.Config{RAGAuditPath: writeRagAuditFixture(t)}}

	out, err := s.humaListRagAuditSessions(context.Background(), &emptyInput{})
	if err != nil {
		t.Fatalf("humaListRagAuditSessions: %v", err)
	}
	if out.Body.Total != 2 {
		t.Fatalf("expected 2 sessions, got %d", out.Body.Total)
	}
	if len(out.Body.Sessions) != 2 {
		t.Fatalf("expected 2 session entries, got %d", len(out.Body.Sessions))
	}
	// sess-1 应聚合到 count 1；first_ts 更早，排在前面。
	if out.Body.Sessions[0].SessionID != "sess-1" {
		t.Fatalf("expected sess-1 first, got %+v", out.Body.Sessions[0])
	}
}
