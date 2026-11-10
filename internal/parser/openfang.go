// ABOUTME: Parses OpenFang JSONL session files under workspaces/*/sessions/.
// ABOUTME: Supports both ~/.openfang and ~/.openfang/workspaces configured roots.
package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// ResolveOpenFangWatchRoots returns the directories that should be watched for
// OpenFang live updates. A configured ~/.openfang root watches workspaces/
// when present; a configured ~/.openfang/workspaces root watches itself.
func ResolveOpenFangWatchRoots(root string) []string {
	if root == "" {
		return nil
	}

	wsRoot := openfangWorkspacesRoot(root)
	if info, err := os.Stat(wsRoot); err == nil && info.IsDir() {
		return []string{wsRoot}
	}

	// If the configured root exists but workspaces/ does not yet, watch the
	// configured root so later workspace creation still generates events.
	if info, err := os.Stat(root); err == nil && info.IsDir() {
		return []string{root}
	}

	return nil
}

// DiscoverOpenFangSessions finds OpenFang session logs under either
// <root>/workspaces/<workspace>/sessions/*.jsonl or
// <root>/<workspace>/sessions/*.jsonl when root already points at workspaces/.
func DiscoverOpenFangSessions(root string) []DiscoveredFile {
	if root == "" {
		return nil
	}

	wsRoot := openfangWorkspacesRoot(root)
	workspaceEntries, err := os.ReadDir(wsRoot)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, wsEntry := range workspaceEntries {
		if !isDirOrSymlink(wsEntry, wsRoot) {
			continue
		}
		workspace := wsEntry.Name()
		if !IsValidOpenFangWorkspace(workspace) {
			continue
		}

		sessionsDir := filepath.Join(wsRoot, workspace, "sessions")
		entries, err := os.ReadDir(sessionsDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			stem, ok := strings.CutSuffix(entry.Name(), ".jsonl")
			if !ok || !IsValidSessionID(stem) {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:    filepath.Join(sessionsDir, entry.Name()),
				Project: workspace,
				Agent:   AgentOpenFang,
			})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindOpenFangSourceFile resolves a raw ID of the form
// "<workspace>:<sessionID>" to an OpenFang JSONL session file.
func FindOpenFangSourceFile(root, rawID string) string {
	if root == "" {
		return ""
	}

	workspace, sessionID, ok := strings.Cut(rawID, ":")
	if !ok || !IsValidOpenFangWorkspace(workspace) ||
		!IsValidSessionID(sessionID) {
		return ""
	}

	candidate := filepath.Join(
		openfangWorkspacesRoot(root),
		workspace,
		"sessions",
		sessionID+".jsonl",
	)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// IsValidOpenFangWorkspace accepts OpenFang workspace names such as
// "delegate-agent" while rejecting path separators and other characters that
// would make the colon-delimited session ID ambiguous.
func IsValidOpenFangWorkspace(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\:~?#%") {
		return false
	}
	if strings.ContainsRune(s, 0) {
		return false
	}
	return true
}

// ParseOpenFangSession parses an OpenFang JSONL session file. Each line is a
// message-like JSON object with top-level timestamp/role/content fields and an
// optional tool_use array containing tool_use and tool_result entries.
func ParseOpenFangSession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	if project == "" {
		project = openfangProjectFromPath(path)
	}

	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if !IsValidOpenFangWorkspace(project) {
		return nil, nil, fmt.Errorf(
			"openfang: invalid workspace %q for %s", project, path,
		)
	}
	if !IsValidSessionID(stem) {
		return nil, nil, fmt.Errorf(
			"openfang: invalid session ID %q for %s", stem, path,
		)
	}

	var (
		messages      []ParsedMessage
		ordinal       int
		malformed     int
		realUserCount int
		startedAt     time.Time
		endedAt       time.Time
		firstMsg      string
	)

	lr := newLineReader(f, maxLineSize)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			malformed++
			continue
		}

		root := gjson.Parse(line)
		role, isSystem, ok := openfangRole(root.Get("role").Str)
		if !ok {
			malformed++
			continue
		}

		content := openfangContentText(root.Get("content"))
		toolCalls, toolResults := openfangToolData(root.Get("tool_use"))
		if strings.TrimSpace(content) == "" &&
			len(toolCalls) == 0 &&
			len(toolResults) == 0 {
			continue
		}

		ts := parseTimestamp(root.Get("timestamp").Str)
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		msg := ParsedMessage{
			Ordinal:       ordinal,
			Role:          role,
			Content:       content,
			Timestamp:     ts,
			HasToolUse:    len(toolCalls) > 0,
			IsSystem:      isSystem,
			ContentLength: len(content),
			ToolCalls:     toolCalls,
			ToolResults:   toolResults,
		}
		messages = append(messages, msg)
		ordinal++

		if firstMsg == "" &&
			role == RoleUser &&
			!isSystem &&
			strings.TrimSpace(content) != "" {
			firstMsg = truncate(
				strings.ReplaceAll(content, "\n", " "),
				300,
			)
		}
		if role == RoleUser &&
			!isSystem &&
			strings.TrimSpace(content) != "" {
			realUserCount++
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(messages) == 0 {
		return nil, nil, nil
	}

	sess := &ParsedSession{
		ID:               "openfang:" + project + ":" + stem,
		Project:          project,
		Machine:          machine,
		Agent:            AgentOpenFang,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: realUserCount,
		MalformedLines:   malformed,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	return sess, messages, nil
}

func openfangWorkspacesRoot(root string) string {
	clean := filepath.Clean(root)
	if filepath.Base(clean) == "workspaces" {
		return root
	}
	candidate := filepath.Join(root, "workspaces")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return root
}

func openfangProjectFromPath(path string) string {
	if filepath.Base(filepath.Dir(path)) != "sessions" {
		return ""
	}
	return filepath.Base(filepath.Dir(filepath.Dir(path)))
}

func openfangRole(role string) (RoleType, bool, bool) {
	switch role {
	case "user":
		return RoleUser, false, true
	case "assistant":
		return RoleAssistant, false, true
	case "system":
		return RoleUser, true, true
	default:
		return "", false, false
	}
}

func openfangContentText(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.Str
	}
	return content.Raw
}

func openfangToolData(
	toolUse gjson.Result,
) ([]ParsedToolCall, []ParsedToolResult) {
	if !toolUse.IsArray() {
		return nil, nil
	}

	var toolCalls []ParsedToolCall
	var toolResults []ParsedToolResult
	toolUse.ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").Str {
		case "tool_use":
			name := item.Get("name").Str
			callID := item.Get("id").Str
			if name == "" && callID == "" {
				return true
			}
			toolCalls = append(toolCalls, ParsedToolCall{
				ToolUseID: callID,
				ToolName:  name,
				Category:  NormalizeToolCategory(name),
				InputJSON: openfangToolInputJSON(item.Get("input")),
			})
		case "tool_result":
			callID := item.Get("tool_use_id").Str
			if callID == "" {
				return true
			}
			raw, contentLen := openfangToolResultPayload(
				item.Get("content"),
			)
			toolResults = append(toolResults, ParsedToolResult{
				ToolUseID:     callID,
				ContentLength: contentLen,
				ContentRaw:    raw,
			})
		}
		return true
	})

	return toolCalls, toolResults
}

func openfangToolInputJSON(input gjson.Result) string {
	if !input.Exists() {
		return "{}"
	}
	if input.Type != gjson.String {
		return input.Raw
	}

	s := strings.TrimSpace(input.Str)
	if s == "" {
		return "{}"
	}
	if json.Valid([]byte(s)) {
		return s
	}
	b, err := json.Marshal(s)
	if err != nil {
		return input.Raw
	}
	return string(b)
}

func openfangToolResultPayload(content gjson.Result) (string, int) {
	if !content.Exists() {
		return "", 0
	}
	if content.Type == gjson.String {
		return content.Raw, len(content.Str)
	}

	text := content.Raw
	if text == "" {
		return "", 0
	}
	b, err := json.Marshal(text)
	if err != nil {
		return "", len(text)
	}
	return string(b), len(text)
}
