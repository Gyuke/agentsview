package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"
)

// writeReport 落一个最小可用的报告 JSON，字段与 rag_bench.py 的契约一致。
func writeReport(t *testing.T, dir, name string, meta map[string]any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	raw, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), raw, 0o644))
}

// reportServer 构造一个已注册路由、但不依赖数据库的 Server。
// 报告相关端点只读文件系统，不需要 store。
func reportServer(t *testing.T, reportsDir string) *Server {
	t.Helper()
	s := &Server{
		cfg: config.Config{BenchmarkReportsDir: reportsDir},
		mux: http.NewServeMux(),
	}
	s.routes()
	return s
}

// reportServerCfg 同上，但允许调用方先调整配置（用于预检/生成等场景）。
func reportServerCfg(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s
}

// 报告目录不存在时不应报错：尚未生成过报告是正常初始状态。
func TestBenchmarkReportsMissingDirIsEmptyList(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	s := reportServer(t, dir)

	w := serveGet(t, s, "/api/v1/rag-audit/benchmark-reports")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body struct {
		Reports   []benchmarkReportMeta `json:"reports"`
		DirExists bool                  `json:"dir_exists"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.False(t, body.DirExists)
	assert.Empty(t, body.Reports)
}

// 列表按文件名（时间戳）倒序，最新在前；非报告文件与坏文件被跳过。
func TestBenchmarkReportsListSortedAndFiltered(t *testing.T) {
	dir := t.TempDir()
	writeReport(t, dir, "2026-09-20_100000.json", map[string]any{
		"generated_at": "2026-09-20T10:00:00Z", "sample_count": 3,
		"summary": map[string]int{"覆盖缺口": 3},
	})
	writeReport(t, dir, "2026-09-20_120000.json", map[string]any{
		"generated_at": "2026-09-20T12:00:00Z", "sample_count": 8,
		"summary": map[string]int{"覆盖缺口": 7, "环境故障": 1},
	})
	// 干扰项：非 json、坏 json、子目录
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2026-09-20_110000.json"), []byte("{bad"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))

	s := reportServer(t, dir)
	w := serveGet(t, s, "/api/v1/rag-audit/benchmark-reports")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body struct {
		Reports   []benchmarkReportMeta `json:"reports"`
		DirExists bool                  `json:"dir_exists"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.True(t, body.DirExists)
	require.Len(t, body.Reports, 2)
	assert.Equal(t, "2026-09-20_120000.json", body.Reports[0].File, "最新在前")
	assert.Equal(t, 8, body.Reports[0].SampleCount)
	assert.Equal(t, 7, body.Reports[0].Summary["覆盖缺口"])
	assert.Equal(t, "2026-09-20_100000.json", body.Reports[1].File)
}

// 报告目录可由 RAGAuditPath 推导：<project>/audit/rag-audit.jsonl
// -> <project>/benchmark/reports
func TestBenchmarkReportsDirDerivedFromAuditPath(t *testing.T) {
	proj := t.TempDir()
	script := filepath.Join(proj, "benchmark", "rag_bench.py")
	require.NoError(t, os.MkdirAll(filepath.Dir(script), 0o755))
	require.NoError(t, os.WriteFile(script, []byte("#!/usr/bin/env python3\n"), 0o755))
	s := &Server{cfg: config.Config{
		RAGAuditPath: filepath.Join(proj, "audit", "rag-audit.jsonl"),
	}}

	want := filepath.Join(proj, "benchmark", "reports")
	assert.Equal(t, want, s.benchmarkReportsDir())
	assert.Equal(t, script, s.benchmarkScriptPath())
	assert.Equal(t, proj, s.benchmarkProjectDir())

	// 显式配置优先于推导
	s.cfg.BenchmarkReportsDir = filepath.Join(proj, "elsewhere")
	s.cfg.BenchmarkScriptPath = filepath.Join(proj, "custom.py")
	s.cfg.BenchmarkProjectDir = filepath.Join(proj, "custom-project")
	assert.Equal(t, filepath.Join(proj, "elsewhere"), s.benchmarkReportsDir())
	assert.Equal(t, s.cfg.BenchmarkScriptPath, s.benchmarkScriptPath())
	assert.Equal(t, s.cfg.BenchmarkProjectDir, s.benchmarkProjectDir())
}

// 详情端点原样透传报告内容。
func TestBenchmarkReportDetailRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeReport(t, dir, "2026-09-20_120000.json", map[string]any{
		"generated_at": "2026-09-20T12:00:00Z",
		"sample_count": 2,
		"items": []map[string]any{
			{"trace_id": "tr-1", "query": "q", "bucket": "覆盖缺口"},
		},
	})
	s := reportServer(t, dir)

	w := serveGet(t, s, "/api/v1/rag-audit/benchmark-report?file=2026-09-20_120000.json")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, float64(2), body["sample_count"])
	assert.NotNil(t, body["items"])
}

func TestBenchmarkReportDetailNotFound(t *testing.T) {
	s := reportServer(t, t.TempDir())
	w := serveGet(t, s, "/api/v1/rag-audit/benchmark-report?file=2026-09-20_130000.json")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// 路径穿越与非法文件名必须被拦下——这是该端点唯一的安全边界。
func TestBenchmarkReportRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	// 在报告目录外放一个文件，若能读到就说明穿越成功。
	outside := filepath.Join(filepath.Dir(dir), "secret.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{"secret":true}`), 0o600))
	s := reportServer(t, dir)

	for _, bad := range []string{
		"../secret.json",
		"..%2Fsecret.json",
		"/etc/passwd",
		"report.json.bak",
		".hidden.json",
		"a/b.json",
	} {
		w := serveGet(t, s, "/api/v1/rag-audit/benchmark-report?file="+bad)
		assert.NotEqual(t, http.StatusOK, w.Code,
			"非法文件名 %q 不应被接受", bad)
		assert.NotContains(t, w.Body.String(), "secret",
			"非法文件名 %q 泄漏了目录外内容", bad)
	}
}

// 未配置任何目录时给出可操作的报错，而不是静默返回空。
func TestBenchmarkReportsUnconfiguredDir(t *testing.T) {
	s := reportServerCfg(t, config.Config{})
	w := serveGet(t, s, "/api/v1/rag-audit/benchmark-reports")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "benchmark_reports_dir")
}

// ---- 生成任务 ----

// 同一时刻只允许一个任务：第二次启动应 409。
func TestBenchmarkRunRejectsConcurrent(t *testing.T) {
	s := reportServerCfg(t, config.Config{})

	// 直接构造「正在运行」状态，避免真的起子进程。
	benchRun.mu.Lock()
	benchRun.running = true
	benchRun.status = "running"
	benchRun.mu.Unlock()
	t.Cleanup(func() {
		benchRun.mu.Lock()
		benchRun.running = false
		benchRun.status = ""
		benchRun.mu.Unlock()
	})

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/rag-audit/benchmark-run", nil)
	assert.Equal(t, http.StatusConflict, w.Code)
}

// 未配置脚本时明确报错，而不是起一个必然失败的子进程。
func TestBenchmarkRunRequiresScriptPath(t *testing.T) {
	s := reportServerCfg(t, config.Config{})
	benchRun.mu.Lock()
	benchRun.running = false
	benchRun.status = ""
	benchRun.mu.Unlock()

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/rag-audit/benchmark-run", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "benchmark_script_path")
}

func TestBenchmarkRunStatusInitiallyIdle(t *testing.T) {
	benchRun.mu.Lock()
	benchRun.running = false
	benchRun.status = ""
	benchRun.mu.Unlock()

	s := reportServer(t, t.TempDir())
	w := serveGet(t, s, "/api/v1/rag-audit/benchmark-run")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body benchmarkRunStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.False(t, body.Running)
	assert.Empty(t, body.Status)
}

func TestBenchmarkRunWritesFreshQueries(t *testing.T) {
	auditPath, fangPath, _ := seedBenchmarkFixture(t)
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "rag_bench.py")
	require.NoError(t, os.WriteFile(script, []byte("#!/usr/bin/env python3\n"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(scriptDir, "queries.yaml"),
		[]byte("stale: true\n"),
		0o644,
	))

	s := benchmarkServer(auditPath, fangPath)
	require.NoError(t, s.writeBenchmarkQueries(context.Background(), script))

	raw, err := os.ReadFile(filepath.Join(scriptDir, "queries.yaml"))
	require.NoError(t, err)
	var doc ragBenchmarkDoc
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	assert.Equal(t, ragBenchmarkSchemaVersion, doc.SchemaVersion)
	assert.NotEmpty(t, doc.ExportedAt)
	assert.Len(t, doc.Queries, 3)
	assert.Equal(t, 5, doc.SourceFingerprint.FeedbackCount)
	assert.Equal(t, 6, doc.SourceFingerprint.AuditCount)
}

// ---- 预检 ----

// 预检必须报出缺失项，而不是笼统的失败——UI 依赖 missing 展示具体原因。
func TestBenchmarkCheckReportsMissingPrereqs(t *testing.T) {
	// 脚本路径指向不存在的文件
	s := reportServerCfg(t, config.Config{
		BenchmarkScriptPath: filepath.Join(t.TempDir(), "absent.py"),
	})

	w := serveGet(t, s, "/api/v1/rag-audit/benchmark-check")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body benchmarkCheck
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.False(t, body.OK)
	assert.Contains(t, body.Missing, "script")
}

// 脚本存在且能找到 codex 时，仅为缺 RAG 服务 —— 验证预检能区分各依赖。
func TestBenchmarkCheckPassesWithScriptAndCodex(t *testing.T) {
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("env not available")
	}
	script := filepath.Join(t.TempDir(), "rag_bench.py")
	require.NoError(t, os.WriteFile(script, []byte(""), 0o644))

	s := reportServerCfg(t, config.Config{BenchmarkScriptPath: script})

	w := serveGet(t, s, "/api/v1/rag-audit/benchmark-check")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body benchmarkCheck
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.NotContains(t, body.Missing, "script", "脚本已存在，不该报缺")
	assert.Equal(t, script, body.Script)
}
