package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// RAG 失败归因报告：读取 rag-demo 侧 harness（benchmark/rag_bench.py）产出的
// reports/*.json 并展示，同时提供一个「生成报告」入口在后台跑该脚本。
//
// 与 benchmark-export 的分工：export 产出喂给 harness 的输入（queries.yaml），
// 本文件消费 harness 的输出（报告 JSON）。两者都不写业务库。
//
// 报告 JSON 的契约由 rag-demo/benchmark/rag_bench.py 定义；此处只做透传与
// 少量元信息提取（列表页用），不改写内容。

// reportFilePattern 限制可读取的文件名，防目录穿越。
// 报告由 harness 以 <YYYY-MM-DD_HHMMSS>.json 命名。
var reportFilePattern = regexp.MustCompile(`^[0-9_-]+\.json$`)

// benchmarkReportMeta 是列表页所需的摘要（从报告 JSON 顶层提取）。
type benchmarkReportMeta struct {
	File        string         `json:"file"`
	GeneratedAt string         `json:"generated_at"`
	SampleCount int            `json:"sample_count"`
	Source      string         `json:"source"`
	Summary     map[string]int `json:"summary"`
	// SizeBytes 供 UI 判断报告是否异常小（可能是坏文件）。
	SizeBytes int64 `json:"size_bytes"`
}

type benchmarkReportList struct {
	Reports []benchmarkReportMeta `json:"reports"`
	Dir     string                `json:"dir"`
	// DirExists 为 false 时 UI 应提示「尚未生成过报告」而不是报错。
	DirExists bool `json:"dir_exists"`
}

type benchmarkReportInput struct {
	File string `query:"file" required:"true" doc:"Report file name, e.g. 2026-09-20_131539.json"`
}

// reportsDir 解析报告目录。显式配置优先；否则由 RAGAuditPath 推导
// （rag-demo 的 audit/ 与 benchmark/reports/ 同级，均在项目根下）。
func (s *Server) benchmarkReportsDir() string {
	if d := strings.TrimSpace(s.cfg.BenchmarkReportsDir); d != "" {
		return d
	}
	if p := strings.TrimSpace(s.cfg.RAGAuditPath); p != "" {
		// <project>/audit/rag-audit.jsonl -> <project>/benchmark/reports
		return filepath.Join(filepath.Dir(filepath.Dir(p)), "benchmark", "reports")
	}
	return ""
}

// benchmarkScriptPath resolves the harness path. Explicit configuration wins;
// otherwise the standard <project>/benchmark/rag_bench.py layout is inferred
// from RAGAuditPath.
func (s *Server) benchmarkScriptPath() string {
	if p := strings.TrimSpace(s.cfg.BenchmarkScriptPath); p != "" {
		return p
	}
	auditPath := strings.TrimSpace(s.cfg.RAGAuditPath)
	if auditPath == "" {
		return ""
	}
	candidate := filepath.Join(
		filepath.Dir(filepath.Dir(auditPath)),
		"benchmark",
		"rag_bench.py",
	)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// benchmarkProjectDir resolves the harness working directory.
func (s *Server) benchmarkProjectDir() string {
	if d := strings.TrimSpace(s.cfg.BenchmarkProjectDir); d != "" {
		return d
	}
	if script := s.benchmarkScriptPath(); script != "" {
		return filepath.Dir(filepath.Dir(script))
	}
	if auditPath := strings.TrimSpace(s.cfg.RAGAuditPath); auditPath != "" {
		return filepath.Dir(filepath.Dir(auditPath))
	}
	return ""
}

func (s *Server) humaListRagBenchmarkReports(
	_ context.Context,
	_ *struct{},
) (*jsonOutput[*benchmarkReportList], error) {
	dir := s.benchmarkReportsDir()
	out := &benchmarkReportList{Reports: []benchmarkReportMeta{}, Dir: dir}
	if dir == "" {
		return nil, apiError(http.StatusBadRequest,
			"未配置报告目录：请设置 benchmark_reports_dir，或设置 rag_audit_path 以便推导")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// 目录不存在不是错误：尚未生成过任何报告。
		if os.IsNotExist(err) {
			return &jsonOutput[*benchmarkReportList]{Body: out}, nil
		}
		return nil, apiError(http.StatusInternalServerError,
			fmt.Sprintf("无法读取报告目录: %v", err))
	}
	out.DirExists = true

	for _, e := range entries {
		if e.IsDir() || !reportFilePattern.MatchString(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // 单个文件读不了就跳过，不影响列表
		}
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		var meta struct {
			GeneratedAt string         `json:"generated_at"`
			SampleCount int            `json:"sample_count"`
			Source      string         `json:"source"`
			Summary     map[string]int `json:"summary"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue // 坏文件跳过
		}
		out.Reports = append(out.Reports, benchmarkReportMeta{
			File:        e.Name(),
			GeneratedAt: meta.GeneratedAt,
			SampleCount: meta.SampleCount,
			Source:      meta.Source,
			Summary:     meta.Summary,
			SizeBytes:   size,
		})
	}
	// 文件名即时间戳，倒序 = 最新在前。
	sort.Slice(out.Reports, func(i, j int) bool {
		return out.Reports[i].File > out.Reports[j].File
	})
	return &jsonOutput[*benchmarkReportList]{Body: out}, nil
}

func (s *Server) humaGetRagBenchmarkReport(
	_ context.Context,
	in *benchmarkReportInput,
) (*bytesOutput, error) {
	dir := s.benchmarkReportsDir()
	if dir == "" {
		return nil, apiError(http.StatusBadRequest, "未配置报告目录")
	}
	if !reportFilePattern.MatchString(in.File) {
		return nil, apiError(http.StatusBadRequest, "非法报告文件名")
	}
	path := filepath.Join(dir, in.File)
	// 双保险：即使正则被绕过，也确保最终路径仍在报告目录内。
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(dir)+string(os.PathSeparator)) {
		return nil, apiError(http.StatusBadRequest, "非法报告路径")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apiError(http.StatusNotFound, "报告不存在")
		}
		return nil, apiError(http.StatusInternalServerError,
			fmt.Sprintf("无法读取报告: %v", err))
	}
	return &bytesOutput{
		ContentType:  "application/json; charset=utf-8",
		CacheControl: "no-store",
		Body:         raw,
	}, nil
}

// ---- 生成任务 ----

// benchmarkRunState 是后台生成任务的运行态。同一时刻只允许一个任务：
// harness 内部有全局限流与模型缓存，并发跑会互相干扰且无意义。
type benchmarkRunState struct {
	mu       sync.Mutex
	running  bool
	started  time.Time
	finished time.Time
	status   string // running | done | failed
	errMsg   string
	// tail 保留脚本输出末尾若干行，失败时用于展示原因。
	tail []string
}

const benchmarkTailLines = 40

var benchRun = &benchmarkRunState{status: ""}

type benchmarkRunStatus struct {
	Status     string   `json:"status"` // "" | running | done | failed
	Running    bool     `json:"running"`
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	ElapsedSec float64  `json:"elapsed_sec"`
	Error      string   `json:"error,omitempty"`
	OutputTail []string `json:"output_tail,omitempty"`
	LastReport string   `json:"last_report,omitempty"`
}

func (st *benchmarkRunState) snapshot() benchmarkRunStatus {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := benchmarkRunStatus{
		Status:     st.status,
		Running:    st.running,
		Error:      st.errMsg,
		OutputTail: append([]string(nil), st.tail...),
	}
	if !st.started.IsZero() {
		out.StartedAt = st.started.UTC().Format(time.RFC3339)
		end := st.finished
		if st.running {
			end = time.Now()
		}
		if !end.IsZero() {
			out.ElapsedSec = end.Sub(st.started).Seconds()
		}
	}
	if !st.finished.IsZero() {
		out.FinishedAt = st.finished.UTC().Format(time.RFC3339)
	}
	return out
}

func (st *benchmarkRunState) appendLine(line string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.tail = append(st.tail, line)
	if len(st.tail) > benchmarkTailLines {
		st.tail = st.tail[len(st.tail)-benchmarkTailLines:]
	}
}

func (s *Server) humaGetRagBenchmarkRun(
	_ context.Context,
	_ *struct{},
) (*jsonOutput[*benchmarkRunStatus], error) {
	st := benchRun.snapshot()
	if st.Status == "done" {
		st.LastReport = s.latestReportFile()
	}
	return &jsonOutput[*benchmarkRunStatus]{Body: &st}, nil
}

// latestReportFile 返回报告目录里最新的文件名（供生成完成后自动加载）。
func (s *Server) latestReportFile() string {
	dir := s.benchmarkReportsDir()
	if dir == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range entries {
		if e.IsDir() || !reportFilePattern.MatchString(e.Name()) {
			continue
		}
		if e.Name() > best {
			best = e.Name()
		}
	}
	return best
}

func (s *Server) humaStartRagBenchmarkRun(
	_ context.Context,
	_ *struct{},
) (*jsonOutput[*benchmarkRunStatus], error) {
	// 并发检查先于配置检查：已有任务在跑时，这才是调用方该得到的答复
	// （而不是被配置问题误导去改配置）。
	benchRun.mu.Lock()
	if benchRun.running {
		benchRun.mu.Unlock()
		return nil, apiError(http.StatusConflict, "已有归因任务在运行中，请等待完成")
	}
	benchRun.mu.Unlock()

	script := s.benchmarkScriptPath()
	if script == "" {
		return nil, apiError(http.StatusBadRequest,
			"未配置 benchmark_script_path（rag_bench.py 的路径）")
	}
	if _, err := os.Stat(script); err != nil {
		return nil, apiError(http.StatusBadRequest,
			fmt.Sprintf("找不到归因脚本: %s", script))
	}

	// 再次加锁置位：上面的检查与这里之间没有 await，但仍然显式重查一次，
	// 保证「检查-置位」是一个原子段（将来若插入其他逻辑也不会漏）。
	benchRun.mu.Lock()
	if benchRun.running {
		benchRun.mu.Unlock()
		return nil, apiError(http.StatusConflict, "已有归因任务在运行中，请等待完成")
	}
	benchRun.running = true
	benchRun.status = "running"
	benchRun.started = time.Now()
	benchRun.finished = time.Time{}
	benchRun.errMsg = ""
	benchRun.tail = nil
	benchRun.mu.Unlock()

	go s.runBenchmark(script)

	st := benchRun.snapshot()
	return &jsonOutput[*benchmarkRunStatus]{Body: &st}, nil
}

// runBenchmark 在后台执行 harness。超时后 kill，避免僵尸任务长期占用。
func (s *Server) runBenchmark(script string) {
	const timeout = 15 * time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := s.writeBenchmarkQueries(ctx, script); err != nil {
		benchRun.mu.Lock()
		benchRun.running = false
		benchRun.finished = time.Now()
		benchRun.status = "failed"
		benchRun.errMsg = fmt.Sprintf("导出 benchmark queries 失败: %v", err)
		benchRun.mu.Unlock()
		return
	}

	cmd := exec.CommandContext(ctx, "python3", script, "--repeat=3")
	if dir := s.benchmarkProjectDir(); dir != "" {
		cmd.Dir = dir
	}
	// 合并 stdout/stderr：harness 的进度与错误混在两边，分开看反而难排查。
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			benchRun.appendLine(line)
		}
	}

	benchRun.mu.Lock()
	defer benchRun.mu.Unlock()
	benchRun.running = false
	benchRun.finished = time.Now()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		benchRun.status = "failed"
		benchRun.errMsg = fmt.Sprintf("生成超时（>%s）", timeout)
	case err != nil:
		benchRun.status = "failed"
		benchRun.errMsg = fmt.Sprintf("脚本退出异常: %v", err)
	default:
		benchRun.status = "done"
		benchRun.errMsg = ""
	}
}

// writeBenchmarkQueries snapshots the current agentsview export next to the
// harness script, which is where rag_bench.py reads queries.yaml.
func (s *Server) writeBenchmarkQueries(ctx context.Context, script string) error {
	out, err := s.humaExportRagBenchmark(ctx, &ragBenchmarkExportInput{
		IncludeAnswer: true,
	})
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(script), "queries.yaml")
	tmp, err := os.CreateTemp(filepath.Dir(path), ".queries-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary queries file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(out.Body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary queries file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary queries file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary queries file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace queries file: %w", err)
	}
	return nil
}

// ---- 依赖预检 ----

type benchmarkCheck struct {
	OK      bool     `json:"ok"`
	Missing []string `json:"missing"`
	Detail  string   `json:"detail,omitempty"`
	Script  string   `json:"script,omitempty"`
	Reports string   `json:"reports_dir,omitempty"`
}

// humaCheckRagBenchmark 在按钮前告知环境是否具备，避免点了才发现缺依赖。
// 探测项与 harness 自身的 fail-fast 预检保持一致。
func (s *Server) humaCheckRagBenchmark(
	_ context.Context,
	_ *struct{},
) (*jsonOutput[*benchmarkCheck], error) {
	res := &benchmarkCheck{
		Missing: []string{},
		Script:  s.benchmarkScriptPath(),
		Reports: s.benchmarkReportsDir(),
	}

	if _, err := exec.LookPath("codex"); err != nil {
		res.Missing = append(res.Missing, "codex")
		res.Detail = "未找到 codex CLI：归因判断依赖它做 LLM 推理"
	}
	if _, err := exec.LookPath("python3"); err != nil {
		res.Missing = append(res.Missing, "python")
		if res.Detail == "" {
			res.Detail = "未找到 python3：无法启动归因脚本"
		}
	}
	script := s.benchmarkScriptPath()
	if script == "" {
		res.Missing = append(res.Missing, "script")
		res.Detail = "未配置 benchmark_script_path"
	} else if _, err := os.Stat(script); err != nil {
		res.Missing = append(res.Missing, "script")
		res.Detail = fmt.Sprintf("找不到归因脚本: %s", script)
	}
	if !s.ragServiceReachable() {
		res.Missing = append(res.Missing, "rag_service")
		if res.Detail == "" {
			res.Detail = "RAG 服务不可达：归因需要读取知识库目录树"
		}
	}
	res.OK = len(res.Missing) == 0
	return &jsonOutput[*benchmarkCheck]{Body: res}, nil
}

// ragServiceReachable 探测 rag-demo 的 MCP 端点。地址与导出功能共用同一份
// 配置来源（rag_bench.py 里硬编码 127.0.0.1:18080，此处保持一致）。
func (s *Server) ragServiceReachable() bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:18080/mcp")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// MCP 端点对 GET 返回 4xx/405 都算「服务活着」——关键是 TCP 能连上。
	return resp.StatusCode < 500
}
