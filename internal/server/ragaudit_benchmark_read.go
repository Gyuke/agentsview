package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Reads backing the RAG benchmark export. Everything here is read-only:
// openfang.db is opened with mode=ro, and the only archive access is the
// verdict table the user writes through the UI.

type benchmarkVerdict struct {
	EffectiveRating string
	Note            string
}

// ragBenchmarkVerdicts loads the user's adjudications, keyed by trace_id.
func (s *Server) ragBenchmarkVerdicts(
	ctx context.Context,
) (map[string]*benchmarkVerdict, error) {
	if s.db == nil {
		return map[string]*benchmarkVerdict{}, nil
	}
	raw, err := s.db.ListBenchmarkVerdicts(ctx)
	if err != nil {
		return nil, internalError("list benchmark verdicts", err)
	}
	out := make(map[string]*benchmarkVerdict, len(raw))
	for traceID, v := range raw {
		out[traceID] = &benchmarkVerdict{
			EffectiveRating: v.EffectiveRating,
			Note:            v.Note,
		}
	}
	return out, nil
}

// readBenchmarkFeedback groups answer_feedback by trace_id and pairs each trace
// with its Feishu question and audit time window.
func (s *Server) readBenchmarkFeedback(
	session string,
	windows map[benchmarkTraceKey]timeRange,
) ([]*benchmarkFeedback, error) {
	db, err := openBenchmarkDB(s.cfg.OpenFangDBPath)
	if err != nil {
		return nil, apiError(http.StatusNotFound,
			fmt.Sprintf("cannot open OpenFang DB: %v", err))
	}
	if db == nil {
		return nil, nil
	}
	defer db.Close()

	query := `SELECT t.trace_id, t.session_id, f.reviewer_open_id, f.rating,
			COALESCE(f.created_at, '')
		FROM answer_feedback f
		JOIN answer_feedback_targets t ON t.answer_id = f.answer_id
		WHERE t.channel = 'feishu'
		  AND f.rating IS NOT NULL AND t.trace_id IS NOT NULL
		  AND t.trace_id != ''`
	args := []any{}
	if session != "" {
		query += " AND t.session_id = ?"
		args = append(args, session)
	}
	query += " ORDER BY f.created_at, f.feedback_id"

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, internalError("query OpenFang answer feedback", err)
	}
	defer rows.Close()

	type key struct{ trace, session string }
	grouped := map[key]*benchmarkFeedback{}
	var order []key
	for rows.Next() {
		var traceID, sessionID, reviewer, rating, createdAt string
		if err := rows.Scan(
			&traceID, &sessionID, &reviewer, &rating, &createdAt,
		); err != nil {
			return nil, internalError("scan OpenFang answer feedback", err)
		}
		k := key{traceID, sessionID}
		fb, ok := grouped[k]
		if !ok {
			fb = &benchmarkFeedback{TraceID: traceID, Session: sessionID}
			grouped[k] = fb
			order = append(order, k)
		}
		fb.Ratings = append(fb.Ratings, benchmarkRating{
			Reviewer:  reviewer,
			Rating:    rating,
			CreatedAt: createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, internalError("iterate OpenFang answer feedback", err)
	}

	// 关联提问与时间窗。
	turns, err := readSessionTurns(db, session)
	if err != nil {
		return nil, err
	}

	out := make([]*benchmarkFeedback, 0, len(order))
	for _, k := range order {
		fb := grouped[k]
		fb.TraceWindow = windows[benchmarkTraceKey{
			traceID:   fb.TraceID,
			sessionID: fb.Session,
		}]
		fb.UserQuestion = questionForTrace(
			turns, fb.Session, fb.TraceWindow,
		)
		if fb.UserQuestion == nil {
			continue
		}
		out = append(out, fb)
	}
	return out, nil
}

// readBenchmarkSessionData opens openfang.db once and returns the user turns
// and the assistant messages per session.
func (s *Server) readBenchmarkSessionData(
	session string,
) (map[string][]sessionQuestion, map[string][]sessionMessage, error) {
	db, err := openBenchmarkDB(s.cfg.OpenFangDBPath)
	if err != nil {
		return nil, nil, apiError(http.StatusNotFound,
			fmt.Sprintf("cannot open OpenFang DB: %v", err))
	}
	if db == nil {
		return nil, nil, nil
	}
	defer db.Close()

	turns, err := readSessionTurns(db, session)
	if err != nil {
		return nil, nil, err
	}
	answers, err := querySessionMessages(db, session)
	if err != nil {
		return nil, nil, err
	}
	return turns, answers, nil
}

// nextQuestionAfter returns the timestamp of the next user turn strictly after
// the round's window end, which bounds the search for the final answer.
//
// The boundary uses every user turn, not just the Feishu-prefixed questions:
// the answer to a Feishu round can be followed by a non-Feishu turn, and using
// only prefixed questions would let the window run on and pick up the wrong
// reply. `[System:` notes are excluded — they are tool-failure annotations, not
// user turns.
func nextQuestionAfter(
	turns []sessionQuestion, window timeRange,
) time.Time {
	if !window.ok {
		return time.Time{}
	}
	var next time.Time
	for _, q := range turns {
		if !q.at.After(window.end) {
			continue
		}
		if next.IsZero() || q.at.Before(next) {
			next = q.at
		}
	}
	return next
}

// toExportRatings converts internal ratings to their YAML form.
func toExportRatings(ratings []benchmarkRating) []ragBenchmarkRating {
	out := make([]ragBenchmarkRating, 0, len(ratings))
	for _, r := range ratings {
		out = append(out, ragBenchmarkRating{
			Reviewer:  r.Reviewer,
			Rating:    r.Rating,
			CreatedAt: r.CreatedAt,
		})
	}
	return out
}

// auditTraceWindows maps trace_id + session_id to [first event, last event]
// from the audit log, used to align openfang session messages with a round.
func auditTraceWindows(lines []ragAuditLine) map[benchmarkTraceKey]timeRange {
	out := map[benchmarkTraceKey]timeRange{}
	for _, line := range lines {
		if line.TraceID == "" || line.SessionID == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, line.Timestamp)
		if err != nil {
			continue
		}
		key := benchmarkTraceKey{
			traceID:   line.TraceID,
			sessionID: line.SessionID,
		}
		w, ok := out[key]
		if !ok {
			out[key] = timeRange{start: ts, end: ts, ok: true}
			continue
		}
		if ts.Before(w.start) {
			w.start = ts
		}
		if ts.After(w.end) {
			w.end = ts
		}
		out[key] = w
	}
	return out
}

// sessionQuestion is one genuine user text turn read from openfang.db.
type sessionQuestion struct {
	session string
	at      time.Time
	text    string
	feishu  bool
}

// partText extracts the text field from a session part payload.
func partText(payload string) string {
	if payload == "" {
		return ""
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return payload
	}
	return decoded.Text
}

// feishuQuestionText recognizes the channel marker written for Feishu user
// input and returns the question without that marker.
func feishuQuestionText(text string) (string, bool) {
	const prefix = "[From: "
	rest, ok := strings.CutPrefix(text, prefix)
	if !ok {
		return "", false
	}
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return "", false
	}
	sender := rest[:end]
	if !strings.HasPrefix(sender, "ou_") || len(sender) == len("ou_") {
		return "", false
	}
	if strings.ContainsAny(sender, " \t\r\n") {
		return "", false
	}
	question := strings.TrimSpace(rest[end+1:])
	if question == "" {
		// A bare marker carries no question; treating it as user input would
		// feed the harness an empty round.
		return "", false
	}
	return question, true
}

func parseOpenFangTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// questionForTrace picks the question that triggered a round: the latest
// genuine user turn at or before the round's first tool call. A later non-Feishu
// turn replaces an older Feishu turn, so the older question cannot be attached
// to a trace it did not trigger.
func questionForTrace(
	bySession map[string][]sessionQuestion, session string, window timeRange,
) *string {
	if !window.ok {
		return nil
	}
	questions := bySession[session]
	if len(questions) == 0 {
		return nil
	}
	var best *sessionQuestion
	for i := range questions {
		q := &questions[i]
		if q.at.After(window.start) {
			continue
		}
		if best == nil || q.at.After(best.at) {
			best = q
		}
	}
	if best == nil {
		return nil
	}
	if !best.feishu {
		return nil
	}
	text := best.text
	return &text
}

// ---- 最终回答：从 openfang 会话消息里取 ----

// sessionMessage is one openfang session message with its parts classified.
type sessionMessage struct {
	session string
	at      time.Time
	// onlyText is true only when every part is a text part. Anything else —
	// tool_use, thinking, images, an unknown future part type, or no parts at
	// all — disqualifies the message as a candidate answer.
	onlyText bool
	texts    []string
}

func querySessionMessages(
	db *sql.DB, session string,
) (map[string][]sessionMessage, error) {
	query := `SELECT m.message_id, m.session_id, m.timestamp, m.role,
			p.part_type, p.payload
		FROM session_messages m
		LEFT JOIN session_parts p ON p.message_id = m.message_id
		WHERE m.role = 'assistant'`
	args := []any{}
	if session != "" {
		query += " AND m.session_id = ?"
		args = append(args, session)
	}
	query += " ORDER BY m.session_id, m.turn_index, p.part_index"

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, internalError("query OpenFang session messages", err)
	}
	defer rows.Close()

	type keyed struct {
		session string
		msgID   string
	}
	byID := map[keyed]*sessionMessage{}
	var order []keyed
	for rows.Next() {
		var msgID, sessionID, role string
		var ts, partType, payload sql.NullString
		if err := rows.Scan(
			&msgID, &sessionID, &ts, &role, &partType, &payload,
		); err != nil {
			return nil, internalError("scan OpenFang session message", err)
		}
		k := keyed{sessionID, msgID}
		msg, ok := byID[k]
		if !ok {
			at, _ := parseOpenFangTime(ts.String)
			msg = &sessionMessage{session: sessionID, at: at, onlyText: true}
			byID[k] = msg
			order = append(order, k)
		}
		switch partType.String {
		case "text":
			if t := partText(payload.String); t != "" {
				msg.texts = append(msg.texts, t)
			}
		default:
			// Any non-text part, including a NULL part_type from the LEFT JOIN
			// (a message with no parts), makes the message ineligible.
			msg.onlyText = false
		}
	}
	if err := rows.Err(); err != nil {
		return nil, internalError("iterate OpenFang session messages", err)
	}

	out := map[string][]sessionMessage{}
	for _, k := range order {
		msg := byID[k]
		out[k.session] = append(out[k.session], *msg)
	}
	return out, nil
}

// answerLookahead bounds how far past a round's last tool call the final answer
// may land when there is no later user turn to provide a hard boundary. The
// reply always comes after the window (the agent digests results and reports
// feedback first), and in practice within seconds.
const answerLookahead = 60 * time.Second

// finalAnswerFor picks the agent's answer for a round.
//
// Search runs from the round's first tool call to the next user turn, so a
// later round's answer is never captured. The next turn is the hard boundary
// even when it is far beyond answerLookahead; the fixed lookahead is only the
// fallback for the last round in a session. Only messages whose parts are all
// text qualify — anything carrying tool_use, thinking, or any other part type
// is progress narration or internal state, not the answer.
func finalAnswerFor(
	bySession map[string][]sessionMessage,
	session string,
	window timeRange,
	nextQuestionAt time.Time,
	maxBytes int,
) string {
	answer, _ := finalAnswerForWithStatus(
		bySession, session, window, nextQuestionAt, maxBytes,
	)
	return answer
}

// finalAnswerForWithStatus returns both the selected answer and a status that
// lets the harness distinguish a missing round from one that only contains
// progress messages.
func finalAnswerForWithStatus(
	bySession map[string][]sessionMessage,
	session string,
	window timeRange,
	nextQuestionAt time.Time,
	maxBytes int,
) (string, string) {
	if !window.ok {
		return "", finalAnswerMissingWindow
	}
	end := window.end.Add(answerLookahead)
	if !nextQuestionAt.IsZero() {
		end = nextQuestionAt
	}
	var best *sessionMessage
	sawAssistant := false
	for i := range bySession[session] {
		m := &bySession[session][i]
		if m.at.Before(window.start) || m.at.After(end) {
			continue
		}
		sawAssistant = true
		if !m.onlyText {
			continue
		}
		if strings.TrimSpace(strings.Join(m.texts, "\n")) == "" {
			continue
		}
		if best == nil || m.at.After(best.at) {
			best = m
		}
	}
	if best == nil {
		if sawAssistant {
			return "", finalAnswerNoEligible
		}
		return "", finalAnswerMissingWindow
	}
	return truncateAnswer(strings.Join(best.texts, "\n"), maxBytes), finalAnswerFound
}

// truncateAnswer caps the answer and marks it, so the export stays small while
// showing the reader that text was removed.
func truncateAnswer(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…[truncated]"
}

// readSessionTurns returns every genuine user text turn in a session. Any new
// user turn bounds the previous round's final-answer search, whichever channel
// it arrived on. The Feishu flag is what allows only `[From: ou_...]` turns to
// become the exported user_question. `[System:` annotations are skipped.
func readSessionTurns(
	db *sql.DB, session string,
) (map[string][]sessionQuestion, error) {
	query := `SELECT m.message_id, m.session_id, m.timestamp, p.payload
		FROM session_parts p
		JOIN session_messages m ON m.message_id = p.message_id
		WHERE m.role = 'user' AND p.part_type = 'text'`
	args := []any{}
	if session != "" {
		query += " AND m.session_id = ?"
		args = append(args, session)
	}
	query += " ORDER BY m.session_id, m.turn_index, p.part_index"

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, internalError("query OpenFang session turns", err)
	}
	defer rows.Close()

	type messageKey struct {
		session string
		message string
	}
	byMessage := map[messageKey]*sessionQuestion{}
	var order []messageKey
	out := map[string][]sessionQuestion{}
	for rows.Next() {
		var messageID, sessionID string
		var ts, payload sql.NullString
		if err := rows.Scan(
			&messageID, &sessionID, &ts, &payload,
		); err != nil {
			return nil, internalError("scan OpenFang session turn", err)
		}
		text := partText(payload.String)
		if strings.HasPrefix(text, "[System:") {
			continue
		}
		at, ok := parseOpenFangTime(ts.String)
		if !ok {
			continue
		}

		key := messageKey{session: sessionID, message: messageID}
		q, exists := byMessage[key]
		if !exists {
			q = &sessionQuestion{session: sessionID, at: at}
			byMessage[key] = q
			order = append(order, key)
		}

		if question, ok := feishuQuestionText(text); ok {
			// A message can contain multiple text parts. The channel marker
			// may follow an earlier text part, so inspect every part before
			// deciding that this user turn is not Feishu.
			if !q.feishu {
				q.text = question
				q.feishu = true
			}
		} else if q.text == "" {
			q.text = strings.TrimSpace(text)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, internalError("iterate OpenFang session turns", err)
	}
	for _, key := range order {
		q := byMessage[key]
		if q.text == "" {
			continue
		}
		out[key.session] = append(out[key.session], *q)
	}
	return out, nil
}
