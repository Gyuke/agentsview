package sync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestEngineClassifyOpenFangPaths(t *testing.T) {
	dbase := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(t.Context(), dbase, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenFang: {root},
		},
		Machine: "local",
	})

	path := filepath.Join(
		root, "workspaces", "delegate-agent", "sessions",
		"35a33e7e-c882-48d4-bb61-f7de5232a710.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))

	gotFiles, err := engine.classifyPaths(t.Context(), []string{path})
	require.NoError(t, err)
	require.Len(t, gotFiles, 1, "path did not classify")
	got := gotFiles[0]
	assert.Equal(t, path, got.Path)
	assert.Equal(t, "delegate-agent", got.Project)
	assert.Equal(t, parser.AgentOpenFang, got.Agent)
	assert.True(t, got.ProviderProcess)
	require.NotNil(t, got.ProviderSource)
}

func TestEngineClassifyOpenFangWorkspacesRootPaths(t *testing.T) {
	dbase := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(t.Context(), dbase, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenFang: {root},
		},
		Machine: "local",
	})

	path := filepath.Join(
		root, "forklift-planner", "sessions",
		"d861c77d-9817-461d-82a2-454bfa698766.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))

	gotFiles, err := engine.classifyPaths(t.Context(), []string{path})
	require.NoError(t, err)
	require.Len(t, gotFiles, 1, "path did not classify")
	got := gotFiles[0]
	assert.Equal(t, "forklift-planner", got.Project)
	assert.Equal(t, parser.AgentOpenFang, got.Agent)
	assert.True(t, got.ProviderProcess)
}

func TestEngineClassifyOpenFangRejectsColonWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("':' is invalid in Windows filenames")
	}

	dbase := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(t.Context(), dbase, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenFang: {root},
		},
		Machine: "local",
	})

	path := filepath.Join(
		root, "workspaces", "bad:workspace", "sessions",
		"35a33e7e-c882-48d4-bb61-f7de5232a710.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))

	got, err := engine.classifyPaths(t.Context(), []string{path})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSyncSingleSession_OpenFangPreservesWorkspaceFromDB(t *testing.T) {
	database := openTestDB(t)

	root := t.TempDir()
	sessDir := filepath.Join(root, "delegate-agent", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	path := filepath.Join(
		sessDir, "35a33e7e-c882-48d4-bb61-f7de5232a710.jsonl",
	)
	require.NoError(t, os.WriteFile(path, []byte(
		`{"timestamp":"2026-06-08T07:45:21.067849277+00:00","role":"user","content":"hi"}`+"\n"+
			`{"timestamp":"2026-06-08T07:45:22.970742774+00:00","role":"assistant","content":"hello"}`,
	), 0o644))

	otherDir := t.TempDir()
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenFang: {otherDir},
		},
		Machine: "local",
	})

	const sessionID = "openfang:delegate-agent:35a33e7e-c882-48d4-bb61-f7de5232a710"
	fp := path
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID:       sessionID,
		Project:  "delegate-agent",
		Machine:  "local",
		Agent:    "openfang",
		FilePath: &fp,
	}))

	require.NoError(t, engine.SyncSingleSession(sessionID))

	got, err := database.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "delegate-agent", got.Project)

	orphan, err := database.GetSession(
		context.Background(),
		"openfang::35a33e7e-c882-48d4-bb61-f7de5232a710",
	)
	require.NoError(t, err)
	assert.Nil(t, orphan)
}
