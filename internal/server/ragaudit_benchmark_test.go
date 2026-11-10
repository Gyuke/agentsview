package server

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
)

// ---- fixtures ----

// newOpenFangFixture creates a minimal openfang.db shaped like the real one:
// only the tables the benchmark export reads.
func newOpenFangFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openfang.db")
	sqlDB, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open fixture db")
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	_, err = sqlDB.Exec(`
CREATE TABLE answer_feedback_targets (
    answer_id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    trace_id TEXT,
    channel TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    question_message_id TEXT NOT NULL,
    answer_message_id TEXT,
    created_at TEXT NOT NULL
);
CREATE TABLE answer_feedback (
    feedback_id TEXT PRIMARY KEY,
    answer_id TEXT NOT NULL,
    reviewer_open_id TEXT NOT NULL,
    rating TEXT NOT NULL,
    reason TEXT,
    comment TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(answer_id, reviewer_open_id)
);
CREATE TABLE session_messages (
    message_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,
    timestamp TEXT,
    turn_index INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(session_id, turn_index)
);
CREATE TABLE session_parts (
    part_id TEXT PRIMARY KEY,
    message_id TEXT NOT NULL,
    part_index INTEGER NOT NULL,
    part_type TEXT NOT NULL,
    payload TEXT NOT NULL,
    metadata TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    UNIQUE(message_id, part_index)
);`)
	require.NoError(t, err, "create fixture schema")
	return sqlDB, path
}

func addFeedbackTarget(
	t *testing.T, db *sql.DB, answerID, sessionID, traceID, channel string,
) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO answer_feedback_targets
			(answer_id, agent_id, session_id, trace_id, channel, chat_id,
			 question_message_id, created_at)
		VALUES (?, 'agent', ?, ?, ?, 'chat', 'qmsg', '2026-09-01T00:00:00Z')`,
		answerID, sessionID, traceID, channel)
	require.NoError(t, err, "insert feedback target")
}

func addFeedback(
	t *testing.T, db *sql.DB, answerID, reviewer, rating, createdAt string,
) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO answer_feedback
			(feedback_id, answer_id, reviewer_open_id, rating, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"fb-"+answerID+"-"+reviewer, answerID, reviewer, rating, createdAt, createdAt)
	require.NoError(t, err, "insert answer feedback")
}

type fixturePart struct {
	partType string
	payload  string
}

func textPart(t *testing.T, text string) fixturePart {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"content_shape": "text",
		"text":          text,
	})
	require.NoError(t, err, "marshal text part")
	return fixturePart{partType: "text", payload: string(raw)}
}

func addFixtureMessage(
	t *testing.T, db *sql.DB, messageID, sessionID, role, ts string,
	turn int, parts ...fixturePart,
) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO session_messages
			(message_id, session_id, role, timestamp, turn_index, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		messageID, sessionID, role, ts, turn, "2026-09-01T00:00:00Z")
	require.NoError(t, err, "insert session message")
	for i, p := range parts {
		_, err := db.Exec(`
			INSERT INTO session_parts
				(part_id, message_id, part_index, part_type, payload, created_at)
			VALUES (?, ?, ?, ?, ?, '2026-09-01T00:00:00Z')`,
			fmt.Sprintf("part-%s-%d", messageID, i), messageID, i, p.partType, p.payload)
		require.NoError(t, err, "insert session part")
	}
}

// searchLine renders one rag-audit search record.
func searchLine(traceID, sessionID, ts, query, docPath string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"origin":"mcp","tool":"rag_search",`+
			`"session_id":%q,"trace_id":%q,"operation":"search",`+
			`"request":{"kb_name":"kb","query":%q},`+
			`"response":{"status":"ok","hit_count":1,"hits":[`+
			`{"score":1,"chunk":{"doc_path":%q,"chunk_id":"c1"}}],`+
			`"candidate_pool":["c1","c2"]}}`,
		ts, sessionID, traceID, query, docPath,
	)
}

func writeAuditFixture(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rag-audit.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644), "write audit fixture")
	return path
}

// seedBenchmarkFixture returns an audit path, openfang path, and the open
// fixture handle. The data covers every consensus bucket plus a non-Feishu
// trace that must never be exported, and every final-answer trap at once.
func seedBenchmarkFixture(t *testing.T) (string, string, *sql.DB) {
	t.Helper()
	sqlDB, fangPath := newOpenFangFixture(t)

	addFeedbackTarget(t, sqlDB, "ans-agreed", "sess-1", "tr-agreed", "feishu")
	addFeedbackTarget(t, sqlDB, "ans-agreed-2", "sess-1", "tr-agreed", "feishu")
	addFeedbackTarget(t, sqlDB, "ans-split", "sess-1", "tr-split", "feishu")
	addFeedbackTarget(t, sqlDB, "ans-split-2", "sess-1", "tr-split", "feishu")
	addFeedbackTarget(t, sqlDB, "ans-single", "sess-2", "tr-single", "feishu")
	addFeedbackTarget(t, sqlDB, "ans-noq", "sess-4", "tr-noq", "feishu")
	addFeedbackTarget(t, sqlDB, "ans-other", "sess-3", "tr-other", "slack")

	addFeedback(t, sqlDB, "ans-agreed", "ou_reviewer_a", "helpful", "2026-09-08T12:00:00Z")
	addFeedback(t, sqlDB, "ans-agreed-2", "ou_reviewer_b", "helpful", "2026-09-09T12:00:00Z")
	addFeedback(t, sqlDB, "ans-split", "ou_reviewer_a", "incorrect", "2026-09-08T12:00:00Z")
	addFeedback(t, sqlDB, "ans-split-2", "ou_reviewer_b", "helpful", "2026-09-17T12:00:00Z")
	addFeedback(t, sqlDB, "ans-single", "ou_reviewer_a", "incorrect", "2026-09-08T12:00:00Z")
	addFeedback(t, sqlDB, "ans-noq", "ou_reviewer_a", "needs_improvement", "2026-09-08T12:00:00Z")
	addFeedback(t, sqlDB, "ans-other", "ou_reviewer_a", "incorrect", "2026-09-08T12:00:00Z")

	// sess-1: a real Feishu question, a later question bounding the first
	// round, and one final answer per round. The trap messages land after the
	// real answer and must lose selection.
	addFixtureMessage(t, sqlDB, "m-user-1", "sess-1", "user",
		"2026-09-08T07:59:00Z", 0,
		textPart(t, "[From: ou_aaa] 3D导航定位精度能到达多少？"))
	addFixtureMessage(t, sqlDB, "m-progress-1", "sess-1", "assistant",
		"2026-09-08T08:00:05Z", 1,
		fixturePart{partType: "tool_use", payload: `{"name":"rag_feedback"}`})
	addFixtureMessage(t, sqlDB, "m-answer-1", "sess-1", "assistant",
		"2026-09-08T08:00:12Z", 2,
		textPart(t, "这是最终回答。"))
	addFixtureMessage(t, sqlDB, "m-user-other", "sess-1", "user",
		"2026-09-08T08:00:15Z", 3,
		textPart(t, "其他 channel 的后续问题"))
	addFixtureMessage(t, sqlDB, "m-answer-other", "sess-1", "assistant",
		"2026-09-08T08:00:20Z", 4,
		textPart(t, "不应越过用户轮次"))
	addFixtureMessage(t, sqlDB, "m-partial-1", "sess-1", "assistant",
		"2026-09-08T08:00:50Z", 5,
		textPart(t, "只含部分文本"), fixturePart{partType: "image", payload: `{}`})
	addFixtureMessage(t, sqlDB, "m-thinking-1", "sess-1", "assistant",
		"2026-09-08T08:00:55Z", 6,
		fixturePart{partType: "thinking", payload: `{"text":"思考中"}`})
	addFixtureMessage(t, sqlDB, "m-user-2", "sess-1", "user",
		"2026-09-08T08:05:00Z", 7,
		textPart(t, "[From: ou_aaa] 第二个问题"))
	addFixtureMessage(t, sqlDB, "m-answer-2", "sess-1", "assistant",
		"2026-09-08T09:00:30Z", 8,
		textPart(t, "第二个回答"))

	addFixtureMessage(t, sqlDB, "m-user-3", "sess-2", "user",
		"2026-09-08T09:59:00Z", 0,
		textPart(t, "[From: ou_bbb] 另一个问题"))
	addFixtureMessage(t, sqlDB, "m-answer-3", "sess-2", "assistant",
		"2026-09-08T10:00:20Z", 1,
		textPart(t, "回答二"))
	// m-answer-2 falls inside this session's timeline by timestamp but belongs
	// to a different session; the export must scope answers per session.
	addFixtureMessage(t, sqlDB, "m-orphan", "sess-4", "assistant",
		"2026-09-08T10:31:00Z", 0,
		textPart(t, "不应被任何提问关联"))

	auditPath := writeAuditFixture(t,
		searchLine("tr-agreed", "sess-1", "2026-09-08T09:00:00Z", "q-agreed", "doc/a"),
		searchLine("tr-split", "sess-1", "2026-09-08T08:00:00Z", "q1", "doc/a"),
		searchLine("tr-split", "sess-1", "2026-09-08T08:00:10Z", "q2", "doc/a"),
		searchLine("tr-single", "sess-2", "2026-09-08T10:00:00Z", "q-single", "doc/b"),
		searchLine("tr-noq", "sess-4", "2026-09-08T10:30:00Z", "q-noq", "doc/c"),
		searchLine("tr-other", "sess-3", "2026-09-08T11:00:00Z", "q-other", "doc/d"),
	)
	return auditPath, fangPath, sqlDB
}

func benchmarkServer(auditPath, fangPath string) *Server {
	return &Server{cfg: config.Config{
		RAGAuditPath:   auditPath,
		OpenFangDBPath: fangPath,
	}}
}

// benchmarkExportInput mirrors what the HTTP layer passes after applying the
// include_answer default.
func benchmarkExportInput() *ragBenchmarkExportInput {
	return &ragBenchmarkExportInput{IncludeAnswer: true}
}

func caseByTrace(t *testing.T, doc ragBenchmarkDoc, traceID string) ragBenchmarkCase {
	t.Helper()
	for _, c := range doc.Queries {
		if c.TraceID == traceID {
			return c
		}
	}
	require.FailNow(t, "trace not found in export", "trace_id=%s", traceID)
	return ragBenchmarkCase{}
}

func ratingsOf(c ragBenchmarkCase) []string {
	out := make([]string, 0, len(c.Ratings))
	for _, r := range c.Ratings {
		out = append(out, r.Rating)
	}
	return out
}

// ---- question marker parsing ----

func TestFeishuQuestionText(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
		ok   bool
	}{
		{name: "genuine question", text: "[From: ou_abc] 精度多少", want: "精度多少", ok: true},
		{name: "marker only", text: "[From: ou_abc]", ok: false},
		{name: "marker with blank question", text: "[From: ou_abc]    ", ok: false},
		{name: "empty sender suffix", text: "[From: ou_] x", ok: false},
		{name: "non user sender", text: "[From: someone] x", ok: false},
		{name: "no marker", text: "普通文本", ok: false},
		{name: "unterminated marker", text: "[From: ou_abc x", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := feishuQuestionText(tc.text)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---- consensus classification ----

func TestConsensusOf(t *testing.T) {
	verdict := &benchmarkVerdict{EffectiveRating: "helpful"}
	tests := []struct {
		name          string
		ratings       []benchmarkRating
		verdict       *benchmarkVerdict
		want          string
		wantEffective *string
	}{
		{
			name:          "single reviewer",
			ratings:       []benchmarkRating{{Rating: "incorrect"}},
			want:          "single",
			wantEffective: benchStrPtr("incorrect"),
		},
		{
			name:          "agreed reviewers",
			ratings:       []benchmarkRating{{Rating: "helpful"}, {Rating: "helpful"}},
			want:          "agreed",
			wantEffective: benchStrPtr("helpful"),
		},
		{
			name:    "split reviewers pick no side",
			ratings: []benchmarkRating{{Rating: "incorrect"}, {Rating: "helpful"}},
			want:    "split",
		},
		{
			name:          "human verdict overrides split",
			ratings:       []benchmarkRating{{Rating: "incorrect"}, {Rating: "helpful"}},
			verdict:       verdict,
			want:          "split",
			wantEffective: benchStrPtr("helpful"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			consensus, effective := consensusOf(tc.ratings, tc.verdict)
			assert.Equal(t, tc.want, consensus)
			if tc.wantEffective == nil {
				assert.Nil(t, effective)
				return
			}
			require.NotNil(t, effective)
			assert.Equal(t, *tc.wantEffective, *effective)
		})
	}
}

func benchStrPtr(s string) *string { return &s }

// ---- answer selection ----

func TestFinalAnswerSkipsNonTextParts(t *testing.T) {
	window := timeRange{
		start: time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC),
		end:   time.Date(2026, 9, 8, 8, 0, 10, 0, time.UTC),
		ok:    true,
	}
	bySession := map[string][]sessionMessage{
		"sess": {
			{
				session: "sess", onlyText: true, texts: []string{"真实回答"},
				at: time.Date(2026, 9, 8, 8, 0, 12, 0, time.UTC),
			},
			{
				// Later, but carries an unknown non-text part: not an answer.
				session: "sess", onlyText: false, texts: []string{"混合内容"},
				at: time.Date(2026, 9, 8, 8, 0, 20, 0, time.UTC),
			},
			{
				// Later, thinking only: not an answer.
				session: "sess", onlyText: false,
				at: time.Date(2026, 9, 8, 8, 0, 30, 0, time.UTC),
			},
		},
	}
	assert.Equal(t, "真实回答",
		finalAnswerFor(bySession, "sess", window, time.Time{}, 4096))

	// A multi-text-part message is joined rather than losing all but the last.
	bySession["sess"] = []sessionMessage{{
		session: "sess", onlyText: true, texts: []string{"第一段", "第二段"},
		at: time.Date(2026, 9, 8, 8, 0, 12, 0, time.UTC),
	}}
	assert.Equal(t, "第一段\n第二段",
		finalAnswerFor(bySession, "sess", window, time.Time{}, 4096))
}

func TestFinalAnswerUsesNextUserTurnBeyondLookahead(t *testing.T) {
	window := timeRange{
		start: time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC),
		end:   time.Date(2026, 9, 8, 8, 0, 10, 0, time.UTC),
		ok:    true,
	}
	nextQuestionAt := time.Date(2026, 9, 8, 8, 5, 0, 0, time.UTC)
	bySession := map[string][]sessionMessage{
		"sess": {{
			session: "sess", onlyText: true, texts: []string{"延迟返回的回答"},
			at: nextQuestionAt.Add(-2 * time.Minute),
		}},
	}

	assert.Equal(t, "延迟返回的回答",
		finalAnswerFor(bySession, "sess", window, nextQuestionAt, 4096),
		"a known next user turn is the hard boundary, not the 60s fallback")

	bySession["sess"][0].at = nextQuestionAt.Add(10 * time.Second)
	assert.Empty(t,
		finalAnswerFor(bySession, "sess", window, nextQuestionAt, 4096),
		"an answer after the next user turn belongs to the next round")
}

func TestQuerySessionMessagesClassifiesParts(t *testing.T) {
	sqlDB, _ := newOpenFangFixture(t)
	addFixtureMessage(t, sqlDB, "m1", "sess", "assistant", "2026-09-08T08:00:12Z", 0,
		textPart(t, "answer"))
	addFixtureMessage(t, sqlDB, "m2", "sess", "assistant", "2026-09-08T08:00:20Z", 1,
		textPart(t, "partial"), fixturePart{partType: "image", payload: `{}`})
	addFixtureMessage(t, sqlDB, "m3", "sess", "assistant", "2026-09-08T08:00:30Z", 2,
		fixturePart{partType: "thinking", payload: `{"text":"hmm"}`})
	addFixtureMessage(t, sqlDB, "m4", "sess", "assistant", "2026-09-08T08:00:40Z", 3,
		textPart(t, "a"), textPart(t, "b"))

	msgs, err := querySessionMessages(sqlDB, "sess")
	require.NoError(t, err)
	require.Len(t, msgs["sess"], 4)
	assert.True(t, msgs["sess"][0].onlyText)
	assert.False(t, msgs["sess"][1].onlyText, "unknown part type must disqualify")
	assert.False(t, msgs["sess"][2].onlyText, "thinking must disqualify")
	assert.True(t, msgs["sess"][3].onlyText)
	assert.Equal(t, []string{"a", "b"}, msgs["sess"][3].texts)
}

func TestReadSessionTurnsFindsFeishuMarkerInAnyTextPart(t *testing.T) {
	sqlDB, _ := newOpenFangFixture(t)
	addFixtureMessage(
		t, sqlDB, "m1", "sess", "user", "2026-09-08T08:00:00Z", 0,
		textPart(t, "普通前缀"),
		textPart(t, "[From: ou_abc] 真正的问题"),
	)

	turns, err := readSessionTurns(sqlDB, "sess")
	require.NoError(t, err)
	require.Len(t, turns["sess"], 1)
	assert.True(t, turns["sess"][0].feishu)
	assert.Equal(t, "真正的问题", turns["sess"][0].text)
}

// ---- export endpoint ----

func TestRagBenchmarkExportFeishuOnlySplitAndStable(t *testing.T) {
	auditPath, fangPath, _ := seedBenchmarkFixture(t)
	s := benchmarkServer(auditPath, fangPath)

	out, err := s.humaExportRagBenchmark(context.Background(), benchmarkExportInput())
	require.NoError(t, err)
	assert.Equal(t, "application/yaml; charset=utf-8", out.ContentType)

	var doc ragBenchmarkDoc
	require.NoError(t, yaml.Unmarshal(out.Body, &doc))
	assert.Equal(t, ragBenchmarkSchemaVersion, doc.SchemaVersion)
	_, err = time.Parse(time.RFC3339, doc.ExportedAt)
	require.NoError(t, err, "exported_at must be RFC3339")
	assert.Equal(t, "agentsview", doc.Source)
	assert.Equal(t, 5, doc.SourceFingerprint.FeedbackCount)
	assert.Equal(t, 6, doc.SourceFingerprint.AuditCount)
	assert.Len(t, doc.SourceFingerprint.QueriesSHA256, 64)
	require.Len(t, doc.Queries, 3, "non-Feishu and input-less rounds must not be exported")
	assert.Equal(t,
		[]string{"tr-agreed", "tr-split", "tr-single"},
		[]string{
			doc.Queries[0].TraceID, doc.Queries[1].TraceID,
			doc.Queries[2].TraceID,
		})

	split := caseByTrace(t, doc, "tr-split")
	assert.Equal(t, "split", split.Consensus)
	assert.Nil(t, split.EffectiveRating)
	assert.Nil(t, split.Rating)
	assert.Equal(t, []string{"incorrect", "helpful"}, ratingsOf(split))
	require.Len(t, split.Ratings, 2)
	assert.Equal(t, "ou_reviewer_a", split.Ratings[0].Reviewer)
	require.NotNil(t, split.UserQuestion)
	assert.Equal(t, "3D导航定位精度能到达多少？", *split.UserQuestion)
	require.NotNil(t, split.FinalAnswer)
	assert.Equal(t, "这是最终回答。", *split.FinalAnswer,
		"later user turns and non-text messages must not win answer selection")
	assert.Equal(t, finalAnswerFound, split.FinalAnswerStatus)
	assert.Equal(t, benchmarkAlignmentTimeWindow, split.Alignment)
	assert.Equal(t, 2, split.Rounds)
	assert.Equal(t, []string{"q1", "q2"}, split.AllQueries)
	assert.Equal(t, "q2", split.LastQuery)
	assert.Equal(t, []string{"doc/a"}, split.HitDocPaths)
	assert.Equal(t, []string{"c1", "c2"}, split.CandidatePool)

	agreed := caseByTrace(t, doc, "tr-agreed")
	assert.Equal(t, "agreed", agreed.Consensus)
	require.NotNil(t, agreed.EffectiveRating)
	assert.Equal(t, "helpful", *agreed.EffectiveRating)
	require.NotNil(t, agreed.Rating)
	assert.Equal(t, "helpful", *agreed.Rating)
	assert.Equal(t, "q-agreed", agreed.LastQuery)
	require.NotNil(t, agreed.FinalAnswer)
	assert.Equal(t, "第二个回答", *agreed.FinalAnswer)

	single := caseByTrace(t, doc, "tr-single")
	assert.Equal(t, "single", single.Consensus)
	require.NotNil(t, single.EffectiveRating)
	assert.Equal(t, "incorrect", *single.EffectiveRating)
	require.NotNil(t, single.Rating)
	assert.Equal(t, "incorrect", *single.Rating)

	assert.NotContains(t, string(out.Body), "tr-noq",
		"a reviewed round without a Feishu question is not a closed-loop case")
	assert.NotContains(t, string(out.Body), "question_missing")

	// Metadata timestamps may differ, but the exported content must be stable.
	again, err := s.humaExportRagBenchmark(context.Background(), benchmarkExportInput())
	require.NoError(t, err)
	var againDoc ragBenchmarkDoc
	require.NoError(t, yaml.Unmarshal(again.Body, &againDoc))
	assert.Equal(t, doc.Queries, againDoc.Queries)
	assert.Equal(t, doc.SourceFingerprint, againDoc.SourceFingerprint)
}

func TestRagBenchmarkExportSkipsStaleFeishuQuestion(t *testing.T) {
	sqlDB, fangPath := newOpenFangFixture(t)
	addFeedbackTarget(t, sqlDB, "ans-stale", "sess-stale", "tr-stale", "feishu")
	addFeedback(t, sqlDB, "ans-stale", "ou_reviewer_a", "incorrect", "2026-09-08T12:00:00Z")

	addFixtureMessage(t, sqlDB, "m-feishu", "sess-stale", "user",
		"2026-09-08T10:00:00Z", 0,
		textPart(t, "[From: ou_aaa] 旧飞书问题"))
	addFixtureMessage(t, sqlDB, "m-other", "sess-stale", "user",
		"2026-09-08T10:10:00Z", 1,
		textPart(t, "其他 channel 的后续问题"))

	auditPath := writeAuditFixture(t,
		searchLine("tr-stale", "sess-stale", "2026-09-08T10:20:00Z", "q-stale", "doc/a"),
	)
	out, err := benchmarkServer(auditPath, fangPath).humaExportRagBenchmark(
		context.Background(), benchmarkExportInput(),
	)
	require.NoError(t, err)

	var doc ragBenchmarkDoc
	require.NoError(t, yaml.Unmarshal(out.Body, &doc))
	assert.Empty(t, doc.Queries,
		"a reviewed trace after a non-Feishu turn must not reuse the older Feishu question")
}

func TestRagBenchmarkExportScopesAuditBySession(t *testing.T) {
	sqlDB, fangPath := newOpenFangFixture(t)
	addFeedbackTarget(t, sqlDB, "ans-scope", "sess-scope", "tr-scope", "feishu")
	addFeedback(t, sqlDB, "ans-scope", "ou_reviewer_a", "incorrect", "2026-09-08T12:00:00Z")

	addFixtureMessage(t, sqlDB, "m-user-scope", "sess-scope", "user",
		"2026-09-08T07:59:00Z", 0,
		textPart(t, "[From: ou_aaa] 会话隔离问题"))
	addFixtureMessage(t, sqlDB, "m-answer-scope", "sess-scope", "assistant",
		"2026-09-08T08:00:05Z", 1,
		textPart(t, "正确会话的回答"))
	addFixtureMessage(t, sqlDB, "m-answer-wrong", "sess-scope", "assistant",
		"2026-09-08T08:30:00Z", 2,
		textPart(t, "同名 trace 的另一个会话延长窗口后不应取到"))

	auditPath := writeAuditFixture(t,
		searchLine("tr-scope", "sess-scope", "2026-09-08T08:00:00Z", "q-scope", "doc/a"),
		searchLine("tr-scope", "sess-other", "2026-09-08T09:00:00Z", "q-other", "doc/b"),
	)
	out, err := benchmarkServer(auditPath, fangPath).humaExportRagBenchmark(
		context.Background(), benchmarkExportInput(),
	)
	require.NoError(t, err)

	var doc ragBenchmarkDoc
	require.NoError(t, yaml.Unmarshal(out.Body, &doc))
	require.Len(t, doc.Queries, 1)
	got := doc.Queries[0]
	assert.Equal(t, []string{"q-scope"}, got.AllQueries)
	assert.Equal(t, []string{"doc/a"}, got.HitDocPaths)
	require.NotNil(t, got.FinalAnswer)
	assert.Equal(t, "正确会话的回答", *got.FinalAnswer)
}

func TestRagBenchmarkExportFiltersAndTruncates(t *testing.T) {
	auditPath, fangPath, _ := seedBenchmarkFixture(t)
	s := benchmarkServer(auditPath, fangPath)

	out, err := s.humaExportRagBenchmark(context.Background(), &ragBenchmarkExportInput{
		Session:       "sess-1",
		Consensus:     "split",
		IncludeAnswer: true,
	})
	require.NoError(t, err)
	var doc ragBenchmarkDoc
	require.NoError(t, yaml.Unmarshal(out.Body, &doc))
	require.Len(t, doc.Queries, 1)
	assert.Equal(t, "tr-split", doc.Queries[0].TraceID)

	// Effective-rating filters exclude unresolved split cases. A raw "helpful"
	// rating on tr-split must not leak into closed-loop attribution.
	out, err = s.humaExportRagBenchmark(context.Background(), &ragBenchmarkExportInput{
		Rating:        "helpful",
		IncludeAnswer: true,
	})
	require.NoError(t, err)
	doc = ragBenchmarkDoc{}
	require.NoError(t, yaml.Unmarshal(out.Body, &doc))
	require.Len(t, doc.Queries, 1)
	assert.Equal(t, "tr-agreed", doc.Queries[0].TraceID)
	assert.Equal(t, 5, doc.SourceFingerprint.FeedbackCount,
		"fingerprint counts the eligible source feedback before rating filters")

	// The raw "incorrect" rating on tr-split is likewise not enough without a
	// human verdict.
	out, err = s.humaExportRagBenchmark(context.Background(), &ragBenchmarkExportInput{
		Rating:        "incorrect",
		IncludeAnswer: true,
	})
	require.NoError(t, err)
	doc = ragBenchmarkDoc{}
	require.NoError(t, yaml.Unmarshal(out.Body, &doc))
	require.Len(t, doc.Queries, 1)
	assert.Equal(t, "tr-single", doc.Queries[0].TraceID)

	// answer_max_bytes truncates without splitting a UTF-8 rune.
	out, err = s.humaExportRagBenchmark(context.Background(), &ragBenchmarkExportInput{
		Session:        "sess-1",
		AnswerMaxBytes: 6,
		IncludeAnswer:  true,
	})
	require.NoError(t, err)
	doc = ragBenchmarkDoc{}
	require.NoError(t, yaml.Unmarshal(out.Body, &doc))
	require.NotNil(t, caseByTrace(t, doc, "tr-split").FinalAnswer)
	assert.True(t,
		strings.HasSuffix(*caseByTrace(t, doc, "tr-split").FinalAnswer, "[truncated]"))
}

func TestSearchQueriesPreservesOrderAndDuplicates(t *testing.T) {
	got := searchQueries([]ragAuditLine{
		{Operation: "search", Request: ragAuditRequest{Query: "q1"}},
		{Operation: "feedback", Request: ragAuditRequest{Query: "ignored"}},
		{Operation: "search", Request: ragAuditRequest{Query: " q2 "}},
		{Operation: "search", Request: ragAuditRequest{Query: "q1"}},
		{Operation: "search", Request: ragAuditRequest{Query: " "}},
	})
	assert.Equal(t, []string{"q1", "q2", "q1"}, got)
}

func TestFinalAnswerStatus(t *testing.T) {
	window := timeRange{
		start: time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC),
		end:   time.Date(2026, 9, 8, 8, 0, 10, 0, time.UTC),
		ok:    true,
	}
	tests := []struct {
		name   string
		msgs   []sessionMessage
		want   string
		status string
	}{
		{
			name: "found",
			msgs: []sessionMessage{{
				session: "sess", onlyText: true, texts: []string{"answer"},
				at: window.end.Add(5 * time.Second),
			}},
			want:   "answer",
			status: finalAnswerFound,
		},
		{
			name: "no eligible text answer",
			msgs: []sessionMessage{{
				session: "sess", onlyText: false, texts: []string{"mixed"},
				at: window.end.Add(5 * time.Second),
			}},
			status: finalAnswerNoEligible,
		},
		{
			name:   "missing window",
			status: finalAnswerMissingWindow,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, status := finalAnswerForWithStatus(
				map[string][]sessionMessage{"sess": tc.msgs},
				"sess", window, time.Time{}, 4096,
			)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.status, status)
		})
	}
}

func TestBenchmarkQueriesSHA256TracksContent(t *testing.T) {
	first, err := benchmarkQueriesSHA256([]ragBenchmarkCase{{TraceID: "trace-a"}})
	require.NoError(t, err)
	same, err := benchmarkQueriesSHA256([]ragBenchmarkCase{{TraceID: "trace-a"}})
	require.NoError(t, err)
	changed, err := benchmarkQueriesSHA256([]ragBenchmarkCase{{TraceID: "trace-b"}})
	require.NoError(t, err)

	assert.Equal(t, first, same)
	assert.NotEqual(t, first, changed)
	assert.Len(t, first, 64)
}

func TestRagBenchmarkExportOpensOpenFangReadOnly(t *testing.T) {
	auditPath, fangPath, sqlDB := seedBenchmarkFixture(t)
	require.NoError(t, sqlDB.Close(), "close fixture writer before locking the file")
	before, err := os.ReadFile(fangPath)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(fangPath, 0o444))

	s := benchmarkServer(auditPath, fangPath)
	_, err = s.humaExportRagBenchmark(context.Background(), benchmarkExportInput())
	require.NoError(t, err, "read-only export must not need write access")

	after, err := os.ReadFile(fangPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "export must not modify openfang.db")
}

func TestRagAuditFeedbackFeishuOnly(t *testing.T) {
	_, fangPath, _ := seedBenchmarkFixture(t)
	s := benchmarkServer("", fangPath)

	out, err := s.humaListRagAuditFeedback(context.Background(), &ragAuditFeedbackInput{})
	require.NoError(t, err)
	require.NotNil(t, out.Body)
	for _, fb := range out.Body.Feedback {
		assert.NotEqual(t, "tr-other", fb.TraceID, "non-Feishu feedback must be hidden")
		assert.NotEmpty(t, fb.Reviewer, "reviewer identity is required to show disagreement")
	}
	assert.Equal(t, 6, out.Body.Total)
}

// ---- verdict endpoint ----

func TestRagBenchmarkVerdictOverridesAndReverts(t *testing.T) {
	auditPath, fangPath, _ := seedBenchmarkFixture(t)
	store := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "archive.db"))
	s := benchmarkServer(auditPath, fangPath)
	s.db = store
	ctx := context.Background()

	// A split case with no verdict stays null.
	out, err := s.humaExportRagBenchmark(ctx, benchmarkExportInput())
	require.NoError(t, err)
	var doc ragBenchmarkDoc
	require.NoError(t, yaml.Unmarshal(out.Body, &doc))
	assert.Nil(t, caseByTrace(t, doc, "tr-split").EffectiveRating)

	_, err = s.humaSetRagBenchmarkVerdict(ctx, &ragBenchmarkVerdictInput{
		Body: ragBenchmarkVerdictBody{
			TraceID:         "tr-split",
			EffectiveRating: "helpful",
			Note:            "回答其实正确",
		},
	})
	require.NoError(t, err)

	out, err = s.humaExportRagBenchmark(ctx, benchmarkExportInput())
	require.NoError(t, err)
	doc = ragBenchmarkDoc{}
	require.NoError(t, yaml.Unmarshal(out.Body, &doc))
	split := caseByTrace(t, doc, "tr-split")
	require.NotNil(t, split.EffectiveRating)
	assert.Equal(t, "helpful", *split.EffectiveRating)
	require.NotNil(t, split.Rating)
	assert.Equal(t, "helpful", *split.Rating)
	assert.Equal(t, "split", split.Consensus, "the verdict must not hide the disagreement")
	assert.Equal(t, []string{"incorrect", "helpful"}, ratingsOf(split))

	// Once adjudicated, the case participates in the effective-rating filter.
	filtered, err := s.humaExportRagBenchmark(ctx, &ragBenchmarkExportInput{
		Rating:        "helpful",
		IncludeAnswer: true,
	})
	require.NoError(t, err)
	doc = ragBenchmarkDoc{}
	require.NoError(t, yaml.Unmarshal(filtered.Body, &doc))
	require.Len(t, doc.Queries, 2)
	assert.Equal(t, []string{"tr-agreed", "tr-split"},
		[]string{doc.Queries[0].TraceID, doc.Queries[1].TraceID})

	list, err := s.humaListRagBenchmarkVerdicts(ctx, &ragBenchmarkVerdictListInput{
		TraceID: "tr-split",
	})
	require.NoError(t, err)
	require.Len(t, list.Body.Verdicts, 1)
	assert.Equal(t, "tr-split", list.Body.Verdicts[0].TraceID)
	assert.Equal(t, "helpful", list.Body.Verdicts[0].EffectiveRating)
	assert.Equal(t, "回答其实正确", list.Body.Verdicts[0].Note)
	assert.NotEmpty(t, list.Body.Verdicts[0].UpdatedAt)

	_, err = s.humaDeleteRagBenchmarkVerdict(ctx, &ragBenchmarkVerdictDeleteInput{
		TraceID: "tr-split",
	})
	require.NoError(t, err)
	out, err = s.humaExportRagBenchmark(ctx, benchmarkExportInput())
	require.NoError(t, err)
	doc = ragBenchmarkDoc{}
	require.NoError(t, yaml.Unmarshal(out.Body, &doc))
	assert.Nil(t, caseByTrace(t, doc, "tr-split").EffectiveRating)

	// Unknown ratings are rejected before touching the archive.
	_, err = s.humaSetRagBenchmarkVerdict(ctx, &ragBenchmarkVerdictInput{
		Body: ragBenchmarkVerdictBody{
			TraceID:         "tr-split",
			EffectiveRating: "bogus",
		},
	})
	require.Error(t, err)
	var apiErr *apiResponseError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Status)
}

func TestRagBenchmarkVerdictRequiresArchiveStore(t *testing.T) {
	s := benchmarkServer("", "")

	_, err := s.humaSetRagBenchmarkVerdict(context.Background(),
		&ragBenchmarkVerdictInput{
			Body: ragBenchmarkVerdictBody{
				TraceID:         "tr-split",
				EffectiveRating: "helpful",
			},
		})
	require.Error(t, err)
	var apiErr *apiResponseError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.Status)

	_, err = s.humaDeleteRagBenchmarkVerdict(context.Background(),
		&ragBenchmarkVerdictDeleteInput{TraceID: "tr-split"})
	require.Error(t, err)
	apiErr = nil
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.Status)
}

// ---- HTTP wiring: defaults, content type, JSON verdicts ----

func TestRagBenchmarkExportHTTPDefaults(t *testing.T) {
	auditPath, fangPath, _ := seedBenchmarkFixture(t)
	store := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "archive.db"))
	s := newRoutedTestServerWithStore(t, store)
	s.cfg.RAGAuditPath = auditPath
	s.cfg.OpenFangDBPath = fangPath

	w := serveGet(t, s, "/api/v1/rag-audit/benchmark-export?session=sess-1")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Type"), "application/yaml")
	assert.Contains(t, w.Body.String(), "这是最终回答。",
		"include_answer defaults to true")

	w = serveGet(t, s, "/api/v1/rag-audit/benchmark-export?session=sess-1&include_answer=false")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "这是最终回答。",
		"include_answer=false drops the answer")
	assert.Contains(t, w.Body.String(), "final_answer_status: not_requested")
}

func TestRagBenchmarkVerdictHTTPReturnsJSON(t *testing.T) {
	store := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, store.SetBenchmarkVerdict("tr-split", "incorrect", "边界情况"))
	s := newRoutedTestServerWithStore(t, store)

	w := serveGet(t, s, "/api/v1/rag-audit/benchmark-verdict?trace_id=tr-split")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	var body ragBenchmarkVerdictList
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Verdicts, 1)
	assert.Equal(t, "tr-split", body.Verdicts[0].TraceID)
	assert.Equal(t, "incorrect", body.Verdicts[0].EffectiveRating)
	assert.Equal(t, "边界情况", body.Verdicts[0].Note)
}
