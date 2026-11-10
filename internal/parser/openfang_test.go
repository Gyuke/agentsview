package parser

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeOpenFangSession(
	t *testing.T, root, workspace, name string, lines []string,
) string {
	t.Helper()
	dir := filepath.Join(root, workspace, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name+".jsonl")
	require.NoError(t, os.WriteFile(
		path,
		[]byte(joinLines(lines)),
		0o644,
	))
	return path
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	out := lines[0]
	for _, line := range lines[1:] {
		out += "\n" + line
	}
	return out
}

func TestResolveOpenFangWatchRoots(t *testing.T) {
	root := t.TempDir()
	wsRoot := filepath.Join(root, "workspaces")
	require.NoError(t, os.MkdirAll(wsRoot, 0o755))

	assert.Equal(t, []string{wsRoot}, ResolveOpenFangWatchRoots(root))
	assert.Equal(t, []string{wsRoot}, ResolveOpenFangWatchRoots(wsRoot))
}

func TestDiscoverOpenFangSessions(t *testing.T) {
	root := t.TempDir()
	wsRoot := filepath.Join(root, "workspaces")
	pathA := writeOpenFangSession(
		t, wsRoot, "assistant",
		"41c8231c-2dde-4cc0-a64c-131e6c0c0d73",
		[]string{
			`{"timestamp":"2026-06-08T07:45:21.067849277+00:00","role":"user","content":"hi"}`,
		},
	)
	pathB := writeOpenFangSession(
		t, wsRoot, "delegate-agent",
		"35a33e7e-c882-48d4-bb61-f7de5232a710",
		[]string{
			`{"timestamp":"2026-06-08T07:45:21.067849277+00:00","role":"user","content":"hello"}`,
		},
	)

	got := DiscoverOpenFangSessions(root)
	require.Len(t, got, 2)
	assert.Equal(t, pathA, got[0].Path)
	assert.Equal(t, "assistant", got[0].Project)
	assert.Equal(t, AgentOpenFang, got[0].Agent)
	assert.Equal(t, pathB, got[1].Path)
	assert.Equal(t, "delegate-agent", got[1].Project)

	provider, ok := NewProvider(
		AgentOpenFang,
		ProviderConfig{Roots: []string{root}, Machine: "local"},
	)
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.Equal(t, pathA, sources[0].DisplayPath)
	assert.Equal(t, "assistant", sources[0].ProjectHint)
	assert.Equal(t, pathB, sources[1].DisplayPath)
	assert.Equal(t, "delegate-agent", sources[1].ProjectHint)
}

func TestDiscoverOpenFangSessionsFromWorkspacesRoot(t *testing.T) {
	root := t.TempDir()
	path := writeOpenFangSession(
		t, root, "forklift-planner",
		"d861c77d-9817-461d-82a2-454bfa698766",
		[]string{
			`{"timestamp":"2026-06-08T07:45:21.067849277+00:00","role":"user","content":"hi"}`,
		},
	)

	got := DiscoverOpenFangSessions(root)
	require.Len(t, got, 1)
	assert.Equal(t, path, got[0].Path)
	assert.Equal(t, "forklift-planner", got[0].Project)
}

func TestFindOpenFangSourceFile(t *testing.T) {
	root := t.TempDir()
	wsRoot := filepath.Join(root, "workspaces")
	path := writeOpenFangSession(
		t, wsRoot, "assistant",
		"41c8231c-2dde-4cc0-a64c-131e6c0c0d73",
		[]string{
			`{"timestamp":"2026-06-08T07:45:21.067849277+00:00","role":"user","content":"hi"}`,
		},
	)

	got := FindOpenFangSourceFile(
		root,
		"assistant:41c8231c-2dde-4cc0-a64c-131e6c0c0d73",
	)
	assert.Equal(t, path, got)
	assert.Empty(t, FindOpenFangSourceFile(root, "assistant:bad:id"))

	provider, ok := NewProvider(
		AgentOpenFang,
		ProviderConfig{Roots: []string{root}, Machine: "local"},
	)
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "assistant:41c8231c-2dde-4cc0-a64c-131e6c0c0d73",
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, path, source.DisplayPath)
	assert.Equal(t, "assistant", source.ProjectHint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  source,
		Machine: "local",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t,
		"openfang:assistant:41c8231c-2dde-4cc0-a64c-131e6c0c0d73",
		outcome.Results[0].Result.Session.ID,
	)
	assert.True(t, outcome.ForceReplace)
}

func TestParseOpenFangSession_BasicUserAssistant(t *testing.T) {
	root := t.TempDir()
	path := writeOpenFangSession(
		t, root, "delegate-agent",
		"35a33e7e-c882-48d4-bb61-f7de5232a710",
		[]string{
			`{"timestamp":"2026-06-08T07:45:21.067849277+00:00","role":"user","content":"你好"}`,
			`{"timestamp":"2026-06-08T07:45:22.970742774+00:00","role":"assistant","content":"你好！我是 delegate-agent。"}`,
		},
	)

	sess, msgs, err := ParseOpenFangSession(path, "delegate-agent", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assertSessionMeta(t, sess,
		"openfang:delegate-agent:35a33e7e-c882-48d4-bb61-f7de5232a710",
		"delegate-agent",
		AgentOpenFang,
	)
	assert.Equal(t, "你好", sess.FirstMessage)
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)

	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "你好", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "你好！我是 delegate-agent。", msgs[1].Content)

	assertTimestamp(t, sess.StartedAt, time.Date(
		2026, 6, 8, 7, 45, 21, 67_849_277, time.UTC,
	))
	assertTimestamp(t, sess.EndedAt, time.Date(
		2026, 6, 8, 7, 45, 22, 970_742_774, time.UTC,
	))
}

func TestParseOpenFangSession_ToolUseAndResult(t *testing.T) {
	root := t.TempDir()
	path := writeOpenFangSession(
		t, root, "assistant",
		"41c8231c-2dde-4cc0-a64c-131e6c0c0d73",
		[]string{
			`{"timestamp":"2026-04-10T11:12:27.786368214+00:00","role":"user","content":"去LM5取货，然后送到LM6"}`,
			`{"timestamp":"2026-04-10T11:12:27.786368214+00:00","role":"assistant","content":"我来帮你创建任务","tool_use":[{"id":"call_36","input":{"name":"nav_to_LM5_pickup","station":"LM5"},"name":"mcp_rbk_rbk_add_navigation_area_block","type":"tool_use"},{"id":"call_bd","input":{"name":"nav_to_LM6_delivery","station":"LM6"},"name":"mcp_rbk_rbk_add_navigation_area_block","type":"tool_use"}]}`,
			`{"timestamp":"2026-04-10T11:12:27.786368214+00:00","role":"user","content":"","tool_use":[{"content":"{\"ok\": true, \"path\": \".runtime/tasks/20260410_191222_034.falcon.json\"}","is_error":false,"tool_use_id":"call_36","type":"tool_result"}]}`,
			`{"timestamp":"2026-04-10T11:12:27.786368214+00:00","role":"assistant","content":"好的，已创建。","tool_use":[{"id":"call_59","input":{"name":"unload_at_LM6"},"name":"mcp_rbk_rbk_add_unload_block","type":"tool_use"}]}`,
		},
	)

	sess, msgs, err := ParseOpenFangSession(path, "assistant", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 4)

	require.Len(t, msgs[1].ToolCalls, 2)
	assert.Equal(t, "call_36", msgs[1].ToolCalls[0].ToolUseID)
	assert.Equal(t,
		"mcp_rbk_rbk_add_navigation_area_block",
		msgs[1].ToolCalls[0].ToolName,
	)
	assert.Contains(t, msgs[1].ToolCalls[0].InputJSON, `"LM5"`)
	assert.True(t, msgs[1].HasToolUse)

	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "call_36", msgs[2].ToolResults[0].ToolUseID)
	assert.Contains(t,
		DecodeContent(msgs[2].ToolResults[0].ContentRaw),
		`"ok": true`,
	)
	assert.Equal(t,
		len(`{"ok": true, "path": ".runtime/tasks/20260410_191222_034.falcon.json"}`),
		msgs[2].ToolResults[0].ContentLength,
	)
}

func TestParseOpenFangSession_MalformedLinesCounted(t *testing.T) {
	root := t.TempDir()
	path := writeOpenFangSession(
		t, root, "assistant",
		"41c8231c-2dde-4cc0-a64c-131e6c0c0d73",
		[]string{
			`not-json`,
			`{"timestamp":"2026-06-08T07:45:21.067849277+00:00","role":"tool","content":"skip me"}`,
			`{"timestamp":"2026-06-08T07:45:21.067849277+00:00","role":"user","content":"hi"}`,
		},
	)

	sess, msgs, err := ParseOpenFangSession(path, "assistant", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)
	assert.Equal(t, 2, sess.MalformedLines)
}

func TestIsValidOpenFangWorkspace(t *testing.T) {
	assert.True(t, IsValidOpenFangWorkspace("delegate-agent"))
	assert.True(t, IsValidOpenFangWorkspace("roboshop-guide"))
	assert.False(t, IsValidOpenFangWorkspace(""))
	assert.False(t, IsValidOpenFangWorkspace("."))
	assert.False(t, IsValidOpenFangWorkspace("bad/name"))
	assert.False(t, IsValidOpenFangWorkspace("bad:name"))
}

func TestDiscoverOpenFangSessionsRejectsColonWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("':' is invalid in Windows filenames")
	}
	root := t.TempDir()
	_ = writeOpenFangSession(
		t, root, "ok-workspace",
		"35a33e7e-c882-48d4-bb61-f7de5232a710",
		[]string{
			`{"timestamp":"2026-06-08T07:45:21.067849277+00:00","role":"user","content":"hi"}`,
		},
	)
	_ = writeOpenFangSession(
		t, root, "bad:workspace",
		"35a33e7e-c882-48d4-bb61-f7de5232a710",
		[]string{
			`{"timestamp":"2026-06-08T07:45:21.067849277+00:00","role":"user","content":"hi"}`,
		},
	)

	got := DiscoverOpenFangSessions(root)
	require.Len(t, got, 1)
	assert.Equal(t, "ok-workspace", got[0].Project)
}
