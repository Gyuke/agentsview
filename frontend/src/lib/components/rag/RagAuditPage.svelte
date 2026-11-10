<script lang="ts">
  import { onMount } from "svelte";
  import {
    Button,
    Checkbox,
    IconButton,
    Modal,
    SegmentedControl,
    TextInput,
    type SegmentedControlOption,
  } from "@kenn-io/kit-ui";
  import { Trash2 } from "@lucide/svelte";
  import { m } from "../../i18n/index.js";
  import { router } from "../../stores/router.svelte.js";
  import { SessionsService } from "../../api/generated/index.js";
  import {
    RagAuditService,
    type BenchmarkCheck as RagAuditBenchmarkCheck,
    type BenchmarkReportMeta as RagAuditReportMeta,
    type BenchmarkRunStatus as RagAuditBenchmarkRunStatus,
    type RagAuditDocStat,
    type RagAuditEvent,
    type RagAuditFeedback,
    type RagAuditSession,
    type RagBenchmarkVerdictItem,
  } from "../../api/generated/index.js";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import RefreshControl from "../shared/RefreshControl.svelte";

  // 右侧时间线当前选中的会话 uuid（audit session_id）。
  let selectedUuid: string | null = $state(null);

  // 左侧会话列表（来自 /rag-audit/sessions 聚合）。
  let auditSessions: RagAuditSession[] = $state([]);
  let sessionsLoading = $state(false);
  let sessionsError: string | null = $state(null);

  // 审计事件（右侧时间线）。
  let events: RagAuditEvent[] = $state([]);
  let eventsLoading = $state(false);
  let eventsError: string | null = $state(null);

  // 时间线展示模式：按 trace_id 分组（一次用户输入触发的一轮 agent loop 工具调用），
  // 还是平铺每条事件。
  let groupByTrace = $state(true);

  // 选中会话的用户消息（角色=user），用于在 trace 组头显示真实用户输入。
  // 从 agentsview 已摄取的 OpenFang 会话拉取；会话未入库时为空，静默降级。
  let userMessages = $state<{ ts: number; content: string }[]>([]);

  // trace_id -> 飞书人工反馈（openfang.db 的 answer_feedback）。同一 trace 必须
  // 保留全部评审记录，才能展示多人分歧并让人工复议。
  let feedbackByTrace = $state(new Map<string, RagAuditFeedback[]>());
  let verdictByTrace = $state(new Map<string, RagBenchmarkVerdictItem>());
  let benchmarkDataLoading = $state(false);
  let benchmarkDataError: string | null = $state(null);
  let benchmarkRequestSeq = 0;
  let pendingOnly = $state(false);

  type VerdictDraft = { rating: string; note: string };
  let verdictDrafts = $state<Record<string, VerdictDraft>>({});
  let savingVerdictTrace = $state<string | null>(null);
  let verdictErrors = $state<Record<string, string>>({});
  let revertingVerdictTrace = $state<string | null>(null);

  // 视图：时间线 / 文档频率 / 归因报告。
  let viewMode = $state<"timeline" | "docs" | "report">("timeline");

  // ---- 归因报告（rag-demo 的 benchmark/rag_bench.py 产出）----
  // 报告与导出是同一链路的两端：export 产出喂给 harness 的输入，
  // 这里是 harness 的输出。报告本身由 harness 计算，前端只做展示。
  type ReportBucket = {
    key: string;
    count: number;
    label: string;
    hint: string;
    tone: "danger" | "warning" | "neutral";
  };
  type ReportItem = {
    trace_id: string;
    session_id: string;
    query: string;
    bucket: string;
    rating?: string | null;
    consensus?: string | null;
    evidence: string;
    fix_type?: string;
    picked_dirs?: string[];
    votes?: Record<string, number>;
    unanimous?: boolean;
    links?: { session?: string; trace?: string };
  };
  type ReportDetail = {
    generated_at?: string;
    source?: string;
    sample_count?: number;
    votes?: number;
    unanimous_count?: number;
    summary?: Record<string, number>;
    fix_type?: Record<string, number>;
    items?: ReportItem[];
    pending_split?: { trace_id: string; ratings: Record<string, number> }[];
  };

  let reports = $state<RagAuditReportMeta[]>([]);
  let reportsDir = $state<string>("");
  let reportsLoadedOnce = $state(false);
  let reportsLoading = $state(false);
  let reportsError = $state<string | null>(null);

  let activeReportFile = $state<string | null>(null);
  let reportDetail = $state<ReportDetail | null>(null);
  let reportLoading = $state(false);
  let reportError = $state<string | null>(null);

  let bucketFilter = $state<string>("all");

  // 生成任务状态（后端后台跑脚本）。
  let runCheck = $state<RagAuditBenchmarkCheck | null>(null);
  let runStatus = $state<RagAuditBenchmarkRunStatus | null>(null);
  let runStarting = $state(false);
  let runError = $state<string | null>(null);
  let runPollTimer: ReturnType<typeof setInterval> | null = null;

  // 删除该会话审计行的确认弹窗：cursor 为会话 uuid，null 表示未打开。
  let deleteCursor: RagAuditSession | null = $state(null);
  let deleting = $state(false);
  let deleteError: string | null = $state(null);

  type TraceDeleteTarget = {
    traceId: string;
    sessionId: string;
    eventCount: number;
    feedbackCount: number;
    hasVerdict: boolean;
  };
  let deleteTraceTarget = $state<TraceDeleteTarget | null>(null);
  let deletingTrace = $state(false);
  let deleteTraceError = $state<string | null>(null);

  // 文档使用频率聚合（全量，独立于所选会话）。
  let docs: RagAuditDocStat[] = $state([]);
  let docsLoading = $state(false);
  let docsError: string | null = $state(null);
  let docsMeta = $state<{
    total_events: number;
    matched_search_events: number;
    distinct_docs: number;
    triggered_docs: number;
  } | null>(null);
  let docsLoadedOnce = $state(false);

  // 文档频率表格排序：默认触发次数降序（与后端一致）。
  type DocSortKey = "trigger" | "consumed" | "ratio" | "last" | "doc";
  let sortKey = $state<DocSortKey>("trigger");
  let sortDir = $state<"asc" | "desc">("desc");

  function setSort(key: DocSortKey) {
    if (sortKey === key) {
      sortDir = sortDir === "desc" ? "asc" : "desc";
      return;
    }
    sortKey = key;
    // 文本/时间列默认升序；数值列默认降序（值越大越值得关注）。
    sortDir = key === "doc" || key === "last" ? "asc" : "desc";
  }

  function sortArrow(key: DocSortKey): string {
    if (sortKey !== key) return "";
    return sortDir === "desc" ? "▼" : "▲";
  }

  // uuid -> agentsview 会话全 id（openfang:<ws>:<uuid>），用于跳转真实会话。
  let sessionIdByUuid = $state(new Map<string, string>());

  let loadingOpenFang = $state(false);
  let lastUpdatedAt = $state<number | null>(null);

  function fmtTime(ts: string | undefined | null): string {
    if (!ts) return "--";
    const d = new Date(ts);
    if (Number.isNaN(d.valueOf())) return ts;
    return d.toLocaleString(undefined, {
      month: "numeric",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
  }

  function shortTs(ts: string | undefined | null): string {
    if (!ts) return "--";
    const d = new Date(ts);
    if (Number.isNaN(d.valueOf())) return ts;
    return d.toLocaleTimeString(undefined, {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
  }

  // trace_id 太长时取前 8 位展示。
  function shortTrace(tid: string): string {
    return tid.length > 8 ? tid.slice(0, 8) : tid;
  }

  function toolLabel(tool?: string | null): string {
    const t = tool ?? "unknown";
    return t
      .replace(/^rag_/, "")
      .replace(/_/g, " ")
      .toUpperCase();
  }

  function strategyLabel(s?: string | null): string {
    return s ?? "dense";
  }

  function ratingLabel(rating: string): string {
    switch (rating) {
      case "helpful":
        return m.rag_audit_rating_helpful();
      case "incorrect":
        return m.rag_audit_rating_incorrect();
      case "needs_improvement":
        return m.rag_audit_rating_needs_improvement();
      default:
        return rating;
    }
  }

  const verdictOptions = $derived<SegmentedControlOption[]>([
    { value: "helpful", label: m.rag_audit_rating_helpful(), tone: "success" },
    {
      value: "incorrect",
      label: m.rag_audit_rating_incorrect(),
      tone: "danger",
    },
    {
      value: "needs_improvement",
      label: m.rag_audit_rating_needs_improvement(),
      tone: "warning",
    },
  ]);

  function consensusLabel(consensus: "single" | "agreed" | "split"): string {
    switch (consensus) {
      case "single":
        return m.rag_audit_consensus_single();
      case "agreed":
        return m.rag_audit_consensus_agreed();
      case "split":
        return m.rag_audit_consensus_split();
    }
  }

  function consensusFor(
    ratings: RagAuditFeedback[],
  ): "single" | "agreed" | "split" {
    if (ratings.length <= 1) return "single";
    const first = ratings[0]?.rating;
    return ratings.every((r) => r.rating === first) ? "agreed" : "split";
  }

  function effectiveRatingFor(
    traceId: string,
    ratings: RagAuditFeedback[],
  ): string | null {
    const verdict = verdictByTrace.get(traceId);
    if (verdict) return verdict.effective_rating;
    const consensus = consensusFor(ratings);
    return consensus === "split" ? null : (ratings[0]?.rating ?? null);
  }

  function reviewerLabel(reviewer: string): string {
    return reviewer.length > 18 ? `${reviewer.slice(0, 18)}...` : reviewer;
  }

  function verdictDraftFor(traceId: string): VerdictDraft {
    const existing = verdictDrafts[traceId];
    if (existing) return existing;
    const verdict = verdictByTrace.get(traceId);
    const ratings = feedbackByTrace.get(traceId) ?? [];
    return {
      rating: verdict?.effective_rating ?? effectiveRatingFor(traceId, ratings) ?? "",
      note: verdict?.note ?? "",
    };
  }

  function updateVerdictDraft(traceId: string, patch: Partial<VerdictDraft>) {
    verdictDrafts = {
      ...verdictDrafts,
      [traceId]: { ...verdictDraftFor(traceId), ...patch },
    };
  }

  function clearVerdictDraft(traceId: string) {
    if (!(traceId in verdictDrafts)) return;
    const next = { ...verdictDrafts };
    delete next[traceId];
    verdictDrafts = next;
  }

  function verdictDirty(g: TraceGroup): boolean {
    const draft = verdictDraftFor(g.traceId);
    const currentRating = g.verdict?.effective_rating ?? g.effectiveRating ?? "";
    const currentNote = g.verdict?.note ?? "";
    return draft.rating !== currentRating || draft.note.trim() !== currentNote;
  }

  function setVerdictError(traceId: string, message: string) {
    verdictErrors = { ...verdictErrors, [traceId]: message };
  }

  function clearVerdictError(traceId: string) {
    if (!verdictErrors[traceId]) return;
    const next = { ...verdictErrors };
    delete next[traceId];
    verdictErrors = next;
  }

  function setPendingOnly(value: boolean) {
    pendingOnly = value;
    if (value) groupByTrace = true;
  }

  async function saveVerdict(g: TraceGroup) {
    const traceId = g.traceId;
    const draft = verdictDraftFor(traceId);
    if (
      !traceId ||
      !draft.rating ||
      savingVerdictTrace === traceId ||
      revertingVerdictTrace === traceId
    ) {
      return;
    }
    savingVerdictTrace = traceId;
    clearVerdictError(traceId);
    try {
      const result = await callGenerated(() =>
        RagAuditService.putApiV1RagAuditBenchmarkVerdict({
          trace_id: traceId,
          effective_rating: draft.rating,
          note: draft.note.trim(),
        }),
      );
      const next = new Map(verdictByTrace);
      next.set(traceId, {
        trace_id: result.trace_id,
        effective_rating: result.effective_rating,
        note: draft.note.trim(),
        updated_at: new Date().toISOString(),
      });
      verdictByTrace = next;
      clearVerdictDraft(traceId);
    } catch (err) {
      if (isAbortError(err)) return;
      setVerdictError(
        traceId,
        err instanceof Error ? err.message : m.rag_audit_verdict_save_error(),
      );
    } finally {
      if (savingVerdictTrace === traceId) savingVerdictTrace = null;
    }
  }

  async function revertVerdict(g: TraceGroup) {
    const traceId = g.traceId;
    if (
      !traceId ||
      !g.verdict ||
      savingVerdictTrace === traceId ||
      revertingVerdictTrace === traceId
    ) {
      return;
    }
    revertingVerdictTrace = traceId;
    clearVerdictError(traceId);
    try {
      await callGenerated(() =>
        RagAuditService.deleteApiV1RagAuditBenchmarkVerdict({
          trace_id: traceId,
        }),
      );
      const next = new Map(verdictByTrace);
      next.delete(traceId);
      verdictByTrace = next;
      clearVerdictDraft(traceId);
    } catch (err) {
      if (isAbortError(err)) return;
      setVerdictError(
        traceId,
        err instanceof Error ? err.message : m.rag_audit_verdict_revert_error(),
      );
    } finally {
      if (revertingVerdictTrace === traceId) revertingVerdictTrace = null;
    }
  }

  function sessionHref(uuid: string): string | undefined {
    const fullId = sessionIdByUuid.get(uuid);
    if (!fullId) return undefined;
    return router.buildSessionHref(fullId);
  }

  async function loadSessions() {
    sessionsLoading = true;
    sessionsError = null;
    try {
      const res = await callGenerated(() =>
        RagAuditService.getApiV1RagAuditSessions(),
      );
      auditSessions = res.sessions ?? [];
      // 若尚无选中且列表非空，默认选第一个。
      if (!selectedUuid && auditSessions.length > 0) {
        selectedUuid = auditSessions[0]!.session_id;
      }
      await loadEvents();
      if (selectedUuid) {
        void loadUserMessages(selectedUuid);
        void loadBenchmarkData(selectedUuid);
      }
    } catch (err) {
      if (isAbortError(err)) return;
      sessionsError =
        err instanceof Error ? err.message : "无法加载 RAG 审计会话列表";
      auditSessions = [];
    } finally {
      sessionsLoading = false;
      lastUpdatedAt = Date.now();
    }
  }

  async function loadOpenFangSessions() {
    loadingOpenFang = true;
    try {
      const res = await callGenerated(() =>
        SessionsService.getApiV1Sessions({
          agent: "openfang",
          limit: 2000,
          // one-shot / automated 会话默认被列表隐藏，但它们的 RAG 审计同样需要
          // 一键跳转，故显式包含。
          include_one_shot: true,
          include_automated: true,
        }),
      );
      const map = new Map<string, string>();
      for (const row of res.sessions ?? []) {
        const id = (row as { id?: unknown }).id;
        if (typeof id !== "string" || !id.startsWith("openfang:")) continue;
        const uuid = id.split(":").pop();
        if (uuid) map.set(uuid, id);
      }
      sessionIdByUuid = map;
      if (selectedUuid) {
        void loadUserMessages(selectedUuid);
      }
    } catch {
      // 打开真实会话的链接是锦上添花；列表失败时保底显示 uuid。
    } finally {
      loadingOpenFang = false;
    }
  }

  async function loadUserMessages(uuid: string) {
    const fullId = sessionIdByUuid.get(uuid);
    if (!fullId) {
      if (selectedUuid === uuid) userMessages = [];
      return;
    }
    try {
      const res = await callGenerated(() =>
        SessionsService.getApiV1SessionsByIdMessages(
          { id: fullId },
          { roles: "user", limit: 1000, direction: "asc" },
        ),
      );
      const msgs = (res.messages ?? [])
        .map((m) => ({ ts: Date.parse(m.timestamp), content: m.content }))
        .filter(
          (m) =>
            !Number.isNaN(m.ts) &&
            // 工具调用失败的 [System: ...] 注记也以 role=user 入库，但其不是真实输入，
            // 排除掉避免把它当成"用户输入"展示。
            !m.content.startsWith("[System:"),
        )
        .sort((a, b) => a.ts - b.ts);
      // 防止快速切换会话时旧响应覆盖新选择。
      if (selectedUuid !== uuid) return;
      userMessages = msgs;
    } catch {
      if (selectedUuid === uuid) userMessages = [];
    }
  }

  // 为一轮 trace 找出触发的用户输入：时间戳不晚于该轮首个工具调用的最近一条用户消息。
  function userInputFor(firstTs: string): string | undefined {
    const t = Date.parse(firstTs);
    if (Number.isNaN(t)) return undefined;
    let best: { ts: number; content: string } | undefined;
    for (const m of userMessages) {
      if (m.ts <= t && (!best || m.ts > best.ts)) best = m;
    }
    return best?.content.trim() || undefined;
  }

  // 拉取所选会话的飞书人工反馈和全局人工裁决。两者都只读；裁决写回
  // agentsview 归档，不回写 openfang.db。
  async function loadBenchmarkData(uuid: string | null) {
    const seq = ++benchmarkRequestSeq;
    benchmarkDataLoading = true;
    benchmarkDataError = null;
    try {
      const [feedbackRes, verdictRes] = await Promise.all([
        uuid
          ? callGenerated(() =>
              RagAuditService.getApiV1RagAuditFeedback({ session_id: uuid }),
            )
          : Promise.resolve(null),
        callGenerated(() =>
          RagAuditService.getApiV1RagAuditBenchmarkVerdict(),
        ),
      ]);
      if (seq !== benchmarkRequestSeq) return;
      if (uuid && selectedUuid !== uuid) return;

      const map = new Map<string, RagAuditFeedback[]>();
      for (const f of feedbackRes?.feedback ?? []) {
        if (!f.trace_id) continue;
        const ratings = map.get(f.trace_id);
        if (ratings) ratings.push(f);
        else map.set(f.trace_id, [f]);
      }
      feedbackByTrace = map;

      const verdicts = new Map<string, RagBenchmarkVerdictItem>();
      for (const verdict of verdictRes.verdicts ?? []) {
        verdicts.set(verdict.trace_id, verdict);
      }
      verdictByTrace = verdicts;
    } catch (err) {
      if (seq !== benchmarkRequestSeq || isAbortError(err)) return;
      benchmarkDataError =
        err instanceof Error ? err.message : m.rag_audit_review_load_error();
      if (!uuid || selectedUuid === uuid) {
        feedbackByTrace = new Map();
        verdictByTrace = new Map();
      }
    } finally {
      if (seq === benchmarkRequestSeq) benchmarkDataLoading = false;
    }
  }

  async function loadEvents() {
    const sid = selectedUuid;
    if (!sid) {
      events = [];
      return;
    }
    eventsLoading = true;
    eventsError = null;
    try {
      const res = await callGenerated(() =>
        RagAuditService.getApiV1RagAudit({
          session_id: sid,
          limit: 1000,
        }),
      );
      events = res.events ?? [];
    } catch (err) {
      if (isAbortError(err)) return;
      eventsError =
        err instanceof Error ? err.message : "无法加载该会话的 RAG 审计事件";
      events = [];
    } finally {
      eventsLoading = false;
    }
  }

  function select(uuid: string) {
    if (selectedUuid === uuid) return;
    // 裁决草稿属于当前会话；切换会话时丢弃未保存编辑，避免回到旧会话时
    // 误以为草稿已经保存。
    verdictDrafts = {};
    verdictErrors = {};
    selectedUuid = uuid;
    void loadEvents();
    void loadUserMessages(uuid);
    void loadBenchmarkData(uuid);
  }

  function askDelete(s: RagAuditSession) {
    deleteCursor = s;
    deleteError = null;
  }

  function closeDelete() {
    if (deleting) return;
    deleteCursor = null;
    deleteError = null;
  }

  // 删除该会话的全部审计行（重写 rag-audit.jsonl）。rag-demo 仍在写入时后端返回
  // 409，错误展示在弹窗内而不关闭，提示用户先停掉 rag-demo。
  async function confirmDelete() {
    const target = deleteCursor;
    if (!target || deleting) return;
    deleting = true;
    deleteError = null;
    try {
      await callGenerated(() =>
        RagAuditService.deleteApiV1RagAuditSession({
          session_id: target.session_id,
        }),
      );
      if (selectedUuid === target.session_id) {
        selectedUuid = null;
        events = [];
        userMessages = [];
        feedbackByTrace = new Map();
        verdictDrafts = {};
        verdictErrors = {};
      }
      deleteCursor = null;
      await loadSessions();
    } catch (err) {
      if (isAbortError(err)) return;
      deleteError =
        err instanceof Error ? err.message : "删除审计数据失败";
    } finally {
      deleting = false;
    }
  }

  function askDeleteTrace(g: TraceGroup) {
    if (!g.traceId || !selectedUuid) return;
    deleteTraceTarget = {
      traceId: g.traceId,
      sessionId: selectedUuid,
      eventCount: g.count,
      feedbackCount: g.ratings.length,
      hasVerdict: !!g.verdict,
    };
    deleteTraceError = null;
  }

  function closeDeleteTrace() {
    if (deletingTrace) return;
    deleteTraceTarget = null;
    deleteTraceError = null;
  }

  async function confirmDeleteTrace() {
    const target = deleteTraceTarget;
    if (!target || deletingTrace) return;
    deletingTrace = true;
    deleteTraceError = null;
    try {
      await callGenerated(() =>
        RagAuditService.deleteApiV1RagAuditTrace({
          trace_id: target.traceId,
          session_id: target.sessionId,
        }),
      );
      const nextFeedback = new Map(feedbackByTrace);
      nextFeedback.delete(target.traceId);
      feedbackByTrace = nextFeedback;
      const nextVerdicts = new Map(verdictByTrace);
      nextVerdicts.delete(target.traceId);
      verdictByTrace = nextVerdicts;
      deleteTraceTarget = null;
      await loadSessions();
      if (!auditSessions.some((s) => s.session_id === target.sessionId)) {
        selectedUuid = null;
        events = [];
        userMessages = [];
        feedbackByTrace = new Map();
        verdictByTrace = new Map();
        return;
      }
      await loadEvents();
      await loadBenchmarkData(target.sessionId);
    } catch (err) {
      if (isAbortError(err)) return;
      deleteTraceError =
        err instanceof Error ? err.message : m.rag_audit_delete_trace_failed();
    } finally {
      deletingTrace = false;
    }
  }

  async function loadDocs() {
    docsLoading = true;
    docsError = null;
    try {
      const res = await callGenerated(() =>
        RagAuditService.getApiV1RagAuditDocs({ top: 100 }),
      );
      docs = (res.docs ?? []) as RagAuditDocStat[];
      docsMeta = res;
      docsLoadedOnce = true;
    } catch (err) {
      if (isAbortError(err)) return;
      docsError = err instanceof Error ? err.message : "无法加载文档使用频率";
      docs = [];
    } finally {
      docsLoading = false;
    }
  }

  // ---- 归因报告：加载与展示 ----

  async function loadReports() {
    reportsLoading = true;
    reportsError = null;
    try {
      const res = await callGenerated(() =>
        RagAuditService.getApiV1RagAuditBenchmarkReports(),
      );
      reports = (res.reports ?? []) as RagAuditReportMeta[];
      reportsDir = res.dir ?? "";
      reportsLoadedOnce = true;
      // 首次加载自动打开最新一份，省去一次点击。
      const newest = reports[0]?.file;
      if (newest && !activeReportFile) {
        await loadReportDetail(newest);
      }
    } catch (err) {
      if (isAbortError(err)) return;
      reportsError =
        err instanceof Error ? err.message : m.rag_report_load_error();
      reports = [];
    } finally {
      reportsLoading = false;
    }
  }

  async function loadReportDetail(file: string) {
    reportLoading = true;
    reportError = null;
    try {
      const raw = await callGenerated(() =>
        RagAuditService.getApiV1RagAuditBenchmarkReport({ file }),
      );
      reportDetail = (typeof raw === "string"
        ? JSON.parse(raw)
        : raw) as ReportDetail;
      activeReportFile = file;
      bucketFilter = "all";
    } catch (err) {
      if (isAbortError(err)) return;
      reportError =
        err instanceof Error ? err.message : m.rag_report_load_error();
      reportDetail = null;
    } finally {
      reportLoading = false;
    }
  }

  async function checkPrereqs() {
    try {
      runCheck = (await callGenerated(() =>
        RagAuditService.getApiV1RagAuditBenchmarkCheck(),
      )) as RagAuditBenchmarkCheck;
    } catch {
      // 预检失败不阻塞页面：按「不可用」处理，由 runError 提示。
      runCheck = null;
    }
  }

  async function generateReport() {
    runStarting = true;
    runError = null;
    try {
      runStatus = (await callGenerated(() =>
        RagAuditService.postApiV1RagAuditBenchmarkRun(),
      )) as RagAuditBenchmarkRunStatus;
      startRunPolling();
    } catch (err) {
      if (isAbortError(err)) return;
      runError = err instanceof Error ? err.message : m.rag_report_run_failed();
    } finally {
      runStarting = false;
    }
  }

  async function refreshRunStatus(
    loadCompletedRun = false,
  ): Promise<RagAuditBenchmarkRunStatus | null> {
    try {
      const st = (await callGenerated(() =>
        RagAuditService.getApiV1RagAuditBenchmarkRun(),
      )) as RagAuditBenchmarkRunStatus;
      runStatus = st;
      if (st.running) startRunPolling();
      if (loadCompletedRun && !st.running && st.status === "done") {
        await loadReports();
        if (st.last_report && activeReportFile !== st.last_report) {
          await loadReportDetail(st.last_report);
        }
      }
      return st;
    } catch {
      return null;
    }
  }

  // 脚本跑完约需数分钟（每条样本要调一次 LLM），用轮询而非长连接。
  function startRunPolling() {
    stopRunPolling();
    runPollTimer = setInterval(() => {
      void pollRun();
    }, 3000);
  }

  function stopRunPolling() {
    if (runPollTimer !== null) {
      clearInterval(runPollTimer);
      runPollTimer = null;
    }
  }

  async function pollRun() {
    const st = await refreshRunStatus(true);
    if (!st || !st.running) {
      stopRunPolling();
    }
  }

  // 三分类展示：召回失败与排序失败行动项相同（都是调检索），合并成一类。
  // 细分仍保留在 items[].bucket 里，需要时可查。
  const reportBuckets = $derived.by<ReportBucket[]>(() => {
    const s = reportDetail?.summary ?? {};
    const pending = (s["需人工裁决"] ?? 0) + (reportDetail?.pending_split?.length ?? 0);
    return [
      {
        key: "coverage",
        count: s["覆盖缺口"] ?? 0,
        label: m.rag_report_bucket_coverage(),
        hint: m.rag_report_bucket_coverage_hint(),
        tone: "danger",
      },
      {
        key: "env",
        count: s["环境故障"] ?? 0,
        label: m.rag_report_bucket_env(),
        hint: m.rag_report_bucket_env_hint(),
        tone: "neutral",
      },
      {
        key: "retrieval",
        count: s["检索问题"] ?? 0,
        label: m.rag_report_bucket_retrieval(),
        hint: m.rag_report_bucket_retrieval_hint(),
        tone: "warning",
      },
      ...(pending > 0
        ? [
            {
              key: "pending",
              count: pending,
              label: m.rag_report_bucket_pending(),
              hint: m.rag_report_bucket_pending_hint(),
              tone: "neutral" as const,
            },
          ]
        : []),
    ];
  });

  // 明细按 bucket 筛选；桶名到 key 的映射用于筛选按钮高亮。
  function bucketKey(bucket: string): string {
    if (bucket === "覆盖缺口") return "coverage";
    if (bucket === "环境故障") return "env";
    if (bucket === "需人工裁决") return "pending";
    return "retrieval";
  }

  function bucketLabel(bucket: string): string {
    switch (bucketKey(bucket)) {
      case "coverage":
        return m.rag_report_bucket_coverage();
      case "env":
        return m.rag_report_bucket_env();
      case "pending":
        return m.rag_report_bucket_pending();
      default:
        return m.rag_report_bucket_retrieval();
    }
  }

  // 预检缺失项 -> 用户可读文案。后端返回稳定标识（codex/script/rag_service）。
  function missingLabel(key: string): string {
    switch (key) {
      case "codex":
        return m.rag_report_missing_codex();
      case "python":
        return m.rag_report_missing_python();
      case "script":
        return m.rag_report_missing_script();
      case "rag_service":
        return m.rag_report_missing_rag();
      default:
        return key;
    }
  }

  const allReportItems = $derived.by<ReportItem[]>(() => {
    const pending = (reportDetail?.pending_split ?? []).map((item) => ({
      trace_id: item.trace_id,
      session_id: "",
      query: item.trace_id,
      bucket: "需人工裁决",
      evidence: m.rag_report_pending_detail(),
      votes: item.ratings,
    }));
    return [...(reportDetail?.items ?? []), ...pending];
  });

  const visibleReportItems = $derived.by<ReportItem[]>(() => {
    const items = allReportItems;
    if (bucketFilter === "all") return items;
    return items.filter((it) => bucketKey(it.bucket) === bucketFilter);
  });

  const reportSourceAge = $derived.by<number | null>(() => {
    const ts = reportDetail?.generated_at;
    if (!ts) return null;
    const d = new Date(ts);
    if (Number.isNaN(d.valueOf())) return null;
    return Math.floor((Date.now() - d.valueOf()) / 86_400_000);
  });

  // 跳转到该条样本所属会话的时间线，并滚动定位到对应 trace。
  // 长会话里手动找 trace 很费事，直接定位省掉翻找。
  function openReportSession(item: ReportItem) {
    if (!item.session_id) return;
    if (!auditSessions.some((s) => s.session_id === item.session_id)) return;
    pendingTraceFocus = item.trace_id || null;
    // 定位目标是一条 trace 组；平铺模式下没有组可落，先切回分组视图。
    // 同时清掉筛选，避免目标被 pendingOnly 过滤掉而找不到锚点。
    if (!groupByTrace) groupByTrace = true;
    if (pendingOnly) pendingOnly = false;
    switchView("timeline");
    select(item.session_id);
  }

  function canOpenReportSession(item: ReportItem): boolean {
    return (
      !!item.session_id &&
      auditSessions.some((s) => s.session_id === item.session_id)
    );
  }

  // 待定位的 trace：select() 触发异步 loadEvents，加载完成后才能滚动，
  // 因此用一个标记位，在 traceGroups 渲染后消费。
  let pendingTraceFocus = $state<string | null>(null);
  // trace id -> 该 trace 组的 DOM 节点，用于 scrollIntoView。
  let traceNodes = new Map<string, HTMLElement>();

  function registerTraceNode(traceId: string, node: HTMLElement | null) {
    if (!traceId) return;
    if (node) traceNodes.set(traceId, node);
    else traceNodes.delete(traceId);
  }

  // Svelte action：trace 组挂载/卸载时登记其 DOM 节点，供滚动定位使用。
  function traceAnchor(node: HTMLElement, traceId: string) {
    registerTraceNode(traceId, node);
    return {
      destroy() {
        registerTraceNode(traceId, null);
      },
    };
  }

  // 命中后短暂高亮，让用户看清落点（否则滚过去了也不知道看哪个）。
  let focusedTrace = $state<string | null>(null);
  let focusTimer: ReturnType<typeof setTimeout> | null = null;

  // 每次 traceGroups 变化后尝试消费待定位标记。等待到目标组出现为止，
  // 因为 select() 之后的加载是异步的（可能先渲染出上一轮的 groups）。
  $effect(() => {
    const target = pendingTraceFocus;
    if (!target) return;
    // 建立对 traceGroups 的依赖，确保加载完成后重跑。
    const ready = traceGroups.some((g) => g.traceId === target);
    if (!ready) return;
    const node = traceNodes.get(target);
    if (!node) return;

    pendingTraceFocus = null;
    node.scrollIntoView({ behavior: "smooth", block: "center" });
    focusedTrace = target;
    if (focusTimer !== null) clearTimeout(focusTimer);
    focusTimer = setTimeout(() => {
      focusedTrace = null;
      focusTimer = null;
    }, 2500);
  });

  function switchView(mode: "timeline" | "docs" | "report") {
    if (viewMode === mode) return;
    viewMode = mode;
    // 首次进入文档频率页签才加载（全局聚合，不随会话切换）。
    if (mode === "docs" && !docsLoadedOnce && !docsLoading) {
      void loadDocs();
    }
    // 归因报告同样按需加载，并顺带跑一次依赖预检（决定按钮是否可用）。
    if (mode === "report") {
      if (!reportsLoadedOnce && !reportsLoading) {
        void loadReports();
      }
      void checkPrereqs();
      void refreshRunStatus(true);
    }
  }

  function refresh() {
    void loadSessions();
    void loadOpenFangSessions();
    void loadDocs();
  }

  const zeroHitCount = $derived(
    events.filter((e) => e.hit_count === 0).length,
  );

  const searchEvents = $derived(
    events.filter((e) => e.operation === "search"),
  );
  const feedbackEvents = $derived(
    events.filter((e) => e.operation === "feedback"),
  );

  // 按当前列/方向排序后的文档副本（不改后端返回的 docs）。
  const sortedDocs = $derived.by<RagAuditDocStat[]>(() => {
    const dir = sortDir === "asc" ? 1 : -1;
    return [...docs].sort((a, b) => {
      let cmp = 0;
      switch (sortKey) {
        case "trigger":
          cmp = a.trigger_count - b.trigger_count;
          break;
        case "consumed":
          cmp = a.consumed_chunk_count - b.consumed_chunk_count;
          break;
        case "ratio":
          cmp = a.consumed_ratio - b.consumed_ratio;
          break;
        case "last":
          cmp = (a.last_triggered ?? "").localeCompare(b.last_triggered ?? "");
          break;
        case "doc":
          cmp = a.doc_path.localeCompare(b.doc_path);
          break;
      }
      // 相等时保持稳定序（后端返回即为触发次数降序）。
      return cmp * dir;
    });
  });

  // 本会话所有"被采用"的 chunk_id（feedback adopted != false），用于检索命中的高亮。
  const acceptedChunkIds = $derived(
    new Set(
      events
        .filter((e) => e.operation === "feedback" && e.adopted !== false)
        .flatMap((e) => e.used_chunk_ids ?? [])
        .map(String),
    ),
  );
  function isAccepted(cid: unknown): boolean {
    return acceptedChunkIds.has(String(cid));
  }

  // 按 trace_id 把事件聚成一轮（一次用户输入触发的 agent loop 工具调用）。
  // 后端事件已按时间升序，Map 保留首次出现顺序；空 trace_id 按各自独立分组。
  type TraceGroup = {
    key: string;
    traceId: string;
    label: string;
    count: number;
    firstTs: string;
    lastTs: string;
    representativeQuery: string;
    userInput?: string;
    ratings: RagAuditFeedback[];
    verdict?: RagBenchmarkVerdictItem;
    consensus: "single" | "agreed" | "split";
    effectiveRating: string | null;
    events: RagAuditEvent[];
  };
  const traceGroups = $derived.by<TraceGroup[]>(() => {
    const map = new Map<string, TraceGroup>();
    for (const ev of events) {
      const tid = ev.trace_id ?? "";
      const key = tid || `untraced-${ev.timestamp}-${ev.tool}-${ev.operation}`;
      let g = map.get(key);
      if (!g) {
        const ratings = tid ? (feedbackByTrace.get(tid) ?? []) : [];
        const verdict = tid ? verdictByTrace.get(tid) : undefined;
        const consensus = consensusFor(ratings);
        g = {
          key,
          traceId: tid,
          label: tid ? shortTrace(tid) : "--",
          count: 0,
          firstTs: ev.timestamp,
          lastTs: ev.timestamp,
          representativeQuery: "",
          userInput: userInputFor(ev.timestamp),
          ratings,
          verdict,
          consensus,
          effectiveRating:
            consensus === "split" ? null : (ratings[0]?.rating ?? null),
          events: [],
        };
        map.set(key, g);
      }
      g.events.push(ev);
      g.count++;
      if (ev.timestamp > g.lastTs) g.lastTs = ev.timestamp;
      if (!g.representativeQuery && ev.query) g.representativeQuery = ev.query;
    }
    return [...map.values()];
  });

  const pendingVerdictCount = $derived(
    traceGroups.filter(
      (group) => group.consensus === "split" && !group.verdict,
    ).length,
  );

  const visibleTraceGroups = $derived(
    pendingOnly
      ? traceGroups.filter(
          (group) => group.consensus === "split" && !group.verdict,
        )
      : traceGroups,
  );

  onMount(() => {
    void loadSessions();
    void loadOpenFangSessions();
    // 从归因报告跳回时间线时会重新挂载，轮询定时器必须随之清理。
    return () => stopRunPolling();
  });
</script>

{#snippet benchmarkGenerateButton()}
  <Button
    surface="outline"
    disabled={runStarting ||
      runStatus?.running === true ||
      (runCheck != null && !runCheck.ok)}
    title={(() => {
      if (runCheck == null) return m.rag_report_checking();
      if (!runCheck.ok) {
        return runCheck.missing.map((k) => missingLabel(k)).join("；");
      }
      return m.rag_report_generate_hint();
    })()}
    onclick={() => void generateReport()}
  >
    {runStatus?.running ? m.rag_report_generating() : m.rag_report_generate()}
  </Button>
{/snippet}

{#snippet benchmarkRunFeedback()}
  {#if runCheck != null && !runCheck.ok}
    <div class="report-warn">
      {runCheck.missing.map((k) => missingLabel(k)).join("；")}
      {#if runCheck.detail}<span class="muted">（{runCheck.detail}）</span>{/if}
    </div>
  {/if}

  {#if runError}
    <div class="report-warn error">{runError}</div>
  {/if}
  {#if runStatus?.status === "running"}
    <div class="report-progress">
      <span class="spinner-dot"></span>
      {m.rag_report_generating()}
      <span class="muted">{Math.round(runStatus.elapsed_sec ?? 0)}s</span>
    </div>
  {/if}
  {#if runStatus?.status === "failed"}
    <div class="report-warn error">
      <strong>{m.rag_report_run_failed()}</strong>
      {#if runStatus.error}<span>：{runStatus.error}</span>{/if}
      {#if runStatus.output_tail?.length}
        <pre class="run-tail">{runStatus.output_tail.slice(-12).join("\n")}</pre>
      {/if}
    </div>
  {/if}
{/snippet}

<div class="ag-page">
  <header class="toolbar">
    <div class="title">
      <h1>RAG 审计</h1>
      <span class="subtitle">
        将 rag-demo 的 audit/rag-audit.jsonl 与 OpenFang 会话关联展示
      </span>
    </div>

    <div class="segment">
      <button
        type="button"
        class:active={viewMode === "timeline"}
        onclick={() => switchView("timeline")}
      >
        会话时间线
      </button>
      <button
        type="button"
        class:active={viewMode === "docs"}
        onclick={() => switchView("docs")}
      >
        文档频率
      </button>
      <button
        type="button"
        class:active={viewMode === "report"}
        onclick={() => switchView("report")}
      >
        {m.rag_report_tab()}
        {#if pendingVerdictCount > 0}
          <span class="tab-badge">{pendingVerdictCount}</span>
        {/if}
      </button>
    </div>

    <RefreshControl
      lastUpdatedAt={lastUpdatedAt}
      busy={sessionsLoading}
      onRefresh={refresh}
      label="刷新"
      title="刷新"
    />
  </header>

  {#if viewMode === "docs"}
    <main class="docs-layout">
      {#if docsLoading && !docs.length}
        <div class="empty">加载文档使用频率…</div>
      {:else if docsError}
        <div class="empty error">{docsError}</div>
      {:else if docs.length === 0}
        <div class="empty">暂无检索命中的文档记录。</div>
      {:else}
        <div class="docs-head">
          <span class="muted">
            参与统计 {docsMeta?.matched_search_events ?? 0} 条检索 ·
            去重 {docsMeta?.distinct_docs ?? 0} 篇 ·
            有触发 {docsMeta?.triggered_docs ?? 0} 篇
          </span>
          <span class="muted">（仅智能体 mcp 调用；采用/检索 = rag_feedback 采用数 ÷ 检索命中数）</span>
        </div>

        <table class="docs-table">
          <thead>
            <tr>
              <th class="sortable">
                <button
                  type="button"
                  class:active={sortKey === "trigger"}
                  onclick={() => setSort("trigger")}
                >
                  触发<span class="sort-arrow">{sortArrow("trigger")}</span>
                </button>
              </th>
              <th class="sortable">
                <button
                  type="button"
                  class:active={sortKey === "consumed"}
                  onclick={() => setSort("consumed")}
                >
                  采用/检索<span class="sort-arrow">{sortArrow("consumed")}</span>
                </button>
              </th>
              <th class="sortable">
                <button
                  type="button"
                  class:active={sortKey === "ratio"}
                  onclick={() => setSort("ratio")}
                >
                  采用率<span class="sort-arrow">{sortArrow("ratio")}</span>
                </button>
              </th>
              <th class="sortable">
                <button
                  type="button"
                  class:active={sortKey === "last"}
                  onclick={() => setSort("last")}
                >
                  最近触发<span class="sort-arrow">{sortArrow("last")}</span>
                </button>
              </th>
              <th class="sortable">
                <button
                  type="button"
                  class:active={sortKey === "doc"}
                  onclick={() => setSort("doc")}
                >
                  文档（kb / doc_path）<span class="sort-arrow">{sortArrow("doc")}</span>
                </button>
              </th>
              <th>示例查询</th>
            </tr>
          </thead>
          <tbody>
            {#each sortedDocs as d (d.kb_name + d.doc_path)}
              <tr>
                <td class="num">{d.trigger_count}</td>
                <td class="num">
                  {d.consumed_chunk_count}/{d.retrieved_chunk_count}
                </td>
                <td class="num">
                  <span class:ratio-empty={d.consumed_ratio === 0}>
                    {(d.consumed_ratio * 100).toFixed(0)}%
                  </span>
                </td>
                <td class="ts-cell">{shortTs(d.last_triggered)}</td>
                <td class="doc-cell" title="{d.kb_name} / {d.doc_path}">
                  <span class="doc-kb">{d.kb_name}</span>
                  <span class="doc-path">{d.doc_path}</span>
                </td>
                <td class="queries">
                  {#if d.example_queries && d.example_queries.length}
                    {#each d.example_queries as q, i (i)}
                      <span class="query-chip" title={q}>{q}</span>
                    {/each}
                  {:else}
                    <span class="muted">—</span>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    </main>
  {:else if viewMode === "report"}
    <main class="report-layout">
      {#if reportsLoading && !reports.length}
        <div class="empty">{m.rag_report_checking()}</div>
      {:else if reportLoading && !reportDetail}
        <div class="empty">{m.rag_report_checking()}</div>
      {:else if reportsError}
        <div class="empty error">{reportsError}</div>
      {:else if reportError}
        <div class="empty error">{reportError}</div>
      {:else if !reportDetail}
        <div class="empty report-empty">
          <p>{m.rag_report_empty()}</p>
          {#if reportsDir}<p class="muted mono">{reportsDir}</p>{/if}
          <div class="report-empty-action">
            {@render benchmarkGenerateButton()}
          </div>
        </div>
        {@render benchmarkRunFeedback()}
      {:else}
        <!-- 头部：数据源 + 生成按钮 -->
        <div class="report-head">
          <div class="report-head-main">
            <h2>{m.rag_report_title()}</h2>
            <p class="muted">{m.rag_report_subtitle()}</p>
            <p class="muted report-source">
              <span>
                {m.rag_report_source({
                  time: fmtTime(reportDetail.generated_at),
                })}
              </span>
              <span>·</span>
              <span>{activeReportFile ?? "--"}</span>
              <span>·</span>
              <span>
                {m.rag_report_samples({
                  count: String(reportDetail.sample_count ?? 0),
                })}
              </span>
              {#if reportSourceAge != null && reportSourceAge > 7}
                <span class="stale">
                  {m.rag_report_stale_warning({
                    days: String(reportSourceAge),
                  })}
                </span>
              {/if}
            </p>
          </div>
          <div class="report-head-actions">
            {#if reports.length > 1}
              <SegmentedControl
                ariaLabel={m.rag_report_history()}
                options={reports.slice(0, 12).map(
                  (r): SegmentedControlOption => ({
                    value: r.file,
                    label: r.file.replace(/\.json$/, ""),
                  }),
                )}
                value={activeReportFile ?? ""}
                onchange={(v) => void loadReportDetail(v)}
              />
            {/if}
            {@render benchmarkGenerateButton()}
          </div>
        </div>

        {@render benchmarkRunFeedback()}

        <!-- KPI：三分法 -->
        <div class="kpi-row">
          {#each reportBuckets as b (b.key)}
            <button
              type="button"
              class="kpi kpi-{b.tone}"
              class:active={bucketFilter === b.key}
              onclick={() =>
                (bucketFilter = bucketFilter === b.key ? "all" : b.key)}
            >
              <span class="kpi-num">{b.count}</span>
              <span class="kpi-label">{b.label}</span>
              <span class="kpi-hint">{b.hint}</span>
            </button>
          {/each}
        </div>

        <!-- 逐条明细 -->
        <div class="report-items">
          <div class="pane-head">
            <span>
              {m.rag_report_items_title()}（{visibleReportItems.length}）
            </span>
            {#if bucketFilter !== "all"}
              <button
                type="button"
                class="link-btn"
                onclick={() => (bucketFilter = "all")}
              >
                {m.rag_report_filter_all()}
              </button>
            {/if}
          </div>
          {#if visibleReportItems.length === 0}
            <div class="empty">{m.rag_report_no_items()}</div>
          {:else}
            {#each visibleReportItems as it (it.trace_id)}
              <article class="report-item">
                <div class="item-head">
                  <span class="bucket-dot bucket-{bucketKey(it.bucket)}"></span>
                  <span class="item-bucket">{bucketLabel(it.bucket)}</span>
                  <span class="item-query">{it.query}</span>
                  {#if it.unanimous === false}
                    <span class="chip warn">{m.rag_report_not_unanimous()}</span>
                  {/if}
                </div>
                <p class="item-evidence">{it.evidence}</p>
                <div class="item-foot">
                  {#if it.fix_type === "new_doc" || it.fix_type === "extend_doc"}
                    <span class="chip">
                      {m.rag_report_fix_hint()}：
                      {it.fix_type === "new_doc"
                        ? m.rag_report_fix_new_doc()
                        : m.rag_report_fix_extend_doc()}
                    </span>
                  {/if}
                  {#if it.votes}
                    <span class="chip">
                      {m.rag_report_votes({
                        votes: Object.entries(it.votes)
                          .map(([k, v]) => `${k}×${v}`)
                          .join(", "),
                      })}
                    </span>
                  {/if}
                  {#if canOpenReportSession(it)}
                    <button
                      type="button"
                      class="link-btn"
                      onclick={() => openReportSession(it)}
                    >
                      {m.rag_report_view_session()}
                    </button>
                  {/if}
                </div>
              </article>
            {/each}
          {/if}
        </div>
      {/if}
    </main>
  {:else}
  <main class="layout">
    <!-- 左：有审计事件的会话列表 -->
    <aside class="session-pane">
      <div class="pane-head">
        <span>会话（{auditSessions.length}）</span>
      </div>

      {#if sessionsLoading && !auditSessions.length}
        <div class="muted">加载中…</div>
      {:else if sessionsError}
        <div class="error">{sessionsError}</div>
        <div class="muted hint">
          后端需通过 AGENTSVIEW_RAG_AUDIT_PATH / rag_audit_path 指向
          audit/rag-audit.jsonl。
        </div>
      {:else if auditSessions.length === 0}
        <div class="muted">暂无带 RAG 审计事件的会话。</div>
      {:else}
        <ul class="session-list">
          {#each auditSessions as s (s.session_id)}
            <li>
              <button
                class="session-row"
                class:active={selectedUuid === s.session_id}
                type="button"
                onclick={() => select(s.session_id)}
              >
                <span class="row-main">
                  <span class="row-id" title={s.session_id}>
                    {s.session_id}
                  </span>
                  <span class="row-meta">
                    {s.event_count} 次 · {shortTs(s.first_ts)} → {shortTs(s.last_ts)}
                  </span>
                </span>
                {#if sessionHref(s.session_id)}
                  <a
                    class="row-open"
                    href={sessionHref(s.session_id)}
                    onclick={(e) => e.stopPropagation()}
                    title="打开真实会话"
                  >
                    打开
                  </a>
                {/if}
                <span
                  class="row-delete"
                  role="button"
                  tabindex="0"
                  title="删除该会话的审计数据"
                  onclick={(e) => {
                    e.stopPropagation();
                    askDelete(s);
                  }}
                  onkeydown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      e.stopPropagation();
                      askDelete(s);
                    }
                  }}
                >
                  删除
                </span>
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </aside>

    <!-- 右：选中会话的时间线 -->
    <section class="timeline-pane">
      {#snippet eventCard(ev: RagAuditEvent)}
        <li class="event">
          <div class="event-rail">
            <span
              class="dot"
              class:zero={ev.operation === "search" && ev.hit_count === 0}
              class:feedback={ev.operation === "feedback"}
            ></span>
          </div>
          <div
            class="card"
            class:zero={ev.operation === "search" && ev.hit_count === 0}
            class:feedback={ev.operation === "feedback"}
          >
            <div class="card-head">
              <span class="ts">{fmtTime(ev.timestamp)}</span>
              <span
                class="badge"
                class:op-search={ev.operation === "search"}
                class:op-feedback={ev.operation === "feedback"}
              >
                {ev.operation === "search" ? "检索" : ev.operation === "feedback" ? "反馈" : "精读"}
              </span>
              <span class="badge origin">{ev.origin}</span>
              <span class="badge tool">{toolLabel(ev.tool)}</span>
              {#if ev.operation === "search" && ev.hit_count === 0}
                <span class="badge zero-badge">零命中</span>
              {/if}
            </div>

            {#if ev.query}
              <div class="query">{ev.query}</div>
            {/if}

            {#if ev.operation === "search"}
              <div class="meta">
                {#if ev.kb_name}<span class="chip">kb: {ev.kb_name}</span>{/if}
                {#if ev.top_k != null}<span class="chip">top_k: {ev.top_k}</span>{/if}
                {#if ev.score_threshold != null}<span class="chip">threshold: {ev.score_threshold}</span>{/if}
                {#if ev.strategy}<span class="chip">策略: {strategyLabel(ev.strategy)}</span>{/if}
                {#if ev.rerank_enabled != null}<span class="chip">rerank: {ev.rerank_enabled ? "开" : "关"}</span>{/if}
                {#if ev.duration_ms != null}<span class="chip">耗时: {ev.duration_ms}ms</span>{/if}
                <span class="chip">命中: {ev.hit_count ?? 0}</span>
              </div>

              {#if ev.hits && ev.hits.length > 0}
                <ul class="hits">
                  {#each ev.hits as h (h.chunk_id + h.score)}
                    <li class="hit" class:accepted={isAccepted(h.chunk_id)}>
                      {#if isAccepted(h.chunk_id)}
                        <span class="hit-score adopted-icon">✓</span>
                      {:else}
                        <span class="hit-score">{h.score.toFixed(3)}</span>
                      {/if}
                      <span class="hit-doc" title={h.doc_path}>{h.doc_path}</span>
                      <span class="hit-chunk" title={h.chunk_id}>{h.chunk_id}</span>
                    </li>
                  {/each}
                </ul>
              {/if}
            {:else if ev.operation === "feedback"}
              <!-- feedback 事件 -->
              <div class="meta">
                <span class="chip">采用: {ev.adopted ? "是" : "否"}</span>
                {#if ev.query}
                  <span class="chip note" title={ev.query}>回应检索: {ev.query}</span>
                {/if}
              </div>
              {#if ev.note}
                <div class="fb-note">{ev.note}</div>
              {/if}
              {#if ev.used_chunk_ids && ev.used_chunk_ids.length > 0}
                <ul class="hits">
                  {#each ev.used_chunk_ids as cid (cid)}
                    <li class="hit accepted">
                      <span class="hit-score adopted-icon">✓</span>
                      <span class="hit-doc" title={cid}>{cid}</span>
                    </li>
                  {/each}
                </ul>
              {/if}
            {:else}
              <!-- 其它操作（如 rag_get_document 精读文档），无"采用"语义 -->
              <div class="meta">
                <span class="chip">精读文档</span>
                {#if ev.hit_count != null}
                  <span class="chip">读取 {ev.hit_count} 篇</span>
                {/if}
              </div>
            {/if}
          </div>
        </li>
      {/snippet}
      {#if !selectedUuid}
        <div class="empty">请选择左侧一个有审计事件的会话。</div>
      {:else if eventsLoading && !events.length}
        <div class="empty">加载时间线…</div>
      {:else if eventsError}
        <div class="empty error">{eventsError}</div>
      {:else if events.length === 0}
        <div class="empty">该会话没有 RAG 审计事件。</div>
      {:else}
        <div class="timeline-head">
          <span>共 {events.length} 条事件</span>
          <span>检索 {searchEvents.length}</span>
          <span>反馈 {feedbackEvents.length}</span>
          {#if zeroHitCount > 0}
            <span class="zero-summary">零命中 {zeroHitCount}</span>
          {/if}
          <Checkbox
            checked={pendingOnly}
            label={m.rag_audit_pending_only()}
            onchange={setPendingOnly}
          />
          {#if pendingVerdictCount > 0}
            <span class="pending-summary">
              {m.rag_audit_pending_count({ count: pendingVerdictCount })}
            </span>
          {/if}
          {#if benchmarkDataLoading}
            <span class="review-loading">{m.rag_audit_review_loading()}</span>
          {:else if benchmarkDataError}
            <span class="review-error">{benchmarkDataError}</span>
          {/if}
          <div class="segment">
            <button
              type="button"
              class:active={groupByTrace}
              onclick={() => (groupByTrace = true)}
            >
              trace 分组
            </button>
            <button
              type="button"
              class:active={!groupByTrace}
              onclick={() => {
                pendingOnly = false;
                groupByTrace = false;
              }}
            >
              平铺
            </button>
          </div>
        </div>

        {#if groupByTrace}
          {#if pendingOnly && visibleTraceGroups.length === 0}
            <div class="empty">{m.rag_audit_pending_empty()}</div>
          {:else}
          <ul class="timeline">
            {#each visibleTraceGroups as g (g.key)}
              <li
                class="trace-group"
                class:focused={focusedTrace != null && g.traceId === focusedTrace}
              >
                <div
                  class="trace-anchor"
                  use:traceAnchor={g.traceId}
                  aria-hidden="true"
                ></div>
                <div class="trace-head">
                  <span class="trace-badge" title={g.traceId}>{g.label}</span>
                  <span class="trace-range"
                    >{shortTs(g.firstTs)} → {shortTs(g.lastTs)}</span
                  >
                  <span class="trace-count">{g.count} 次调用</span>
                  {#if g.representativeQuery && g.representativeQuery !== g.userInput}
                    <span class="trace-query" title={g.representativeQuery}
                      >「{g.representativeQuery}」</span
                    >
                  {/if}
                  {#if g.traceId && selectedUuid}
                    <span class="trace-actions">
                      <IconButton
                        size="sm"
                        tone="danger"
                        title={m.rag_audit_delete_trace()}
                        ariaLabel={m.rag_audit_delete_trace()}
                        onclick={() => askDeleteTrace(g)}
                      >
                        <Trash2 size={14} aria-hidden="true" />
                      </IconButton>
                    </span>
                  {/if}
                </div>
                {#if g.userInput}
                  <div class="trace-user" title={g.userInput}>
                    用户输入：{g.userInput}
                  </div>
                {/if}
                {#if g.ratings.length > 0}
                  <section class="review-panel" class:review-split={g.consensus === "split"}>
                    <div class="review-head">
                      <span class="review-title">
                        {m.rag_audit_reviewer_feedback()}
                      </span>
                      <span
                        class="consensus-badge"
                        class:consensus-split={g.consensus === "split"}
                      >
                        {consensusLabel(g.consensus)}
                      </span>
                      {#if g.consensus === "split" && !g.verdict}
                        <span class="pending-badge">
                          {m.rag_audit_pending_badge()}
                        </span>
                      {/if}
                      {#if g.verdict}
                        <span class="verdict-badge">
                          {m.rag_audit_human_verdict()}:
                          {ratingLabel(g.verdict.effective_rating)}
                        </span>
                      {/if}
                    </div>

                    <ul class="ratings">
                      {#each g.ratings as rating, index (`${rating.reviewer}-${rating.created_at}-${index}`)}
                        <li class="rating-row" class:rating-bad={rating.rating !== "helpful"}>
                          <span class="fb-rating">{ratingLabel(rating.rating)}</span>
                          <span class="fb-reviewer">
                            {m.rag_audit_reviewer({
                              reviewer: reviewerLabel(rating.reviewer),
                            })}
                          </span>
                          <span class="fb-time">{shortTs(rating.created_at)}</span>
                          {#if rating.reason}
                            <span class="fb-reason">{rating.reason}</span>
                          {/if}
                          {#if rating.comment}
                            <span class="fb-comment" title={rating.comment}>
                              {rating.comment}
                            </span>
                          {/if}
                        </li>
                      {/each}
                    </ul>

                    {#if g.consensus === "split" && !g.verdict}
                      <p class="review-help">{m.rag_audit_split_help()}</p>
                    {/if}

                    {#if g.verdict}
                      <div class="verdict-result">
                        <span class="verdict-result-label">
                          {m.rag_audit_human_verdict()}
                        </span>
                        <span class="verdict-result-rating">
                          {ratingLabel(g.verdict.effective_rating)}
                        </span>
                        {#if g.verdict.note}
                          <span class="verdict-result-note" title={g.verdict.note}>
                            {g.verdict.note}
                          </span>
                        {/if}
                        <span class="fb-time">{shortTs(g.verdict.updated_at)}</span>
                      </div>
                    {/if}

                    <div class="verdict-editor">
                      <span class="field-label">
                        {m.rag_audit_human_verdict()}
                      </span>
                      <SegmentedControl
                        options={verdictOptions}
                        value={verdictDraftFor(g.traceId).rating}
                        ariaLabel={m.rag_audit_human_verdict()}
                        onchange={(value) =>
                          updateVerdictDraft(g.traceId, { rating: value })}
                      />
                      <TextInput
                        block
                        class="verdict-note"
                        value={verdictDraftFor(g.traceId).note}
                        ariaLabel={m.rag_audit_verdict_note_placeholder()}
                        placeholder={m.rag_audit_verdict_note_placeholder()}
                        oninput={(value) =>
                          updateVerdictDraft(g.traceId, { note: value })}
                      />
                      <div class="verdict-actions">
                        <Button
                          size="sm"
                          tone="info"
                          surface="solid"
                          label={savingVerdictTrace === g.traceId
                            ? m.rag_audit_verdict_saving()
                            : m.rag_audit_verdict_save()}
                          disabled={!verdictDraftFor(g.traceId).rating ||
                            !verdictDirty(g) ||
                            savingVerdictTrace === g.traceId ||
                            revertingVerdictTrace === g.traceId}
                          onclick={() => void saveVerdict(g)}
                        />
                        {#if g.verdict}
                          <Button
                            size="sm"
                            tone="danger"
                            surface="outline"
                            label={revertingVerdictTrace === g.traceId
                              ? m.rag_audit_verdict_reverting()
                              : m.rag_audit_verdict_revert()}
                            disabled={savingVerdictTrace === g.traceId ||
                              revertingVerdictTrace === g.traceId}
                            onclick={() => void revertVerdict(g)}
                          />
                        {/if}
                      </div>
                      {#if verdictErrors[g.traceId]}
                        <p class="verdict-error">{verdictErrors[g.traceId]}</p>
                      {/if}
                    </div>
                  </section>
                {/if}
                <ul class="trace-events">
                  {#each g.events as ev (ev.timestamp + ev.tool + ev.operation)}
                    {@render eventCard(ev)}
                  {/each}
                </ul>
              </li>
            {/each}
          </ul>
          {/if}
        {:else}
          <ul class="timeline">
            {#each events as ev (ev.timestamp + ev.tool + ev.operation)}
              {@render eventCard(ev)}
            {/each}
          </ul>
        {/if}
      {/if}
    </section>
  </main>
  {/if}

  {#snippet deleteActions()}
    <span class="confirm-actions">
      <Button
        label="取消"
        tone="neutral"
        surface="outline"
        disabled={deleting}
        onclick={closeDelete}
      />
      <Button
        label={deleting ? "删除中…" : "删除审计数据"}
        tone="danger"
        surface="solid"
        disabled={deleting}
        onclick={confirmDelete}
      />
    </span>
  {/snippet}

  {#snippet deleteTraceActions()}
    <span class="confirm-actions">
      <Button
        label="取消"
        tone="neutral"
        surface="outline"
        disabled={deletingTrace}
        onclick={closeDeleteTrace}
      />
      <Button
        label={deletingTrace
          ? m.rag_audit_delete_trace_deleting()
          : m.rag_audit_delete_trace_confirm()}
        tone="danger"
        surface="solid"
        disabled={deletingTrace}
        onclick={() => void confirmDeleteTrace()}
      />
    </span>
  {/snippet}

  {#if deleteCursor}
    <Modal
      title="删除该会话的审计数据"
      closeLabel="关闭"
      tone="danger"
      width="420px"
      onclose={closeDelete}
      footer={deleteActions}
    >
      <p class="confirm-message">
        将从 rag-audit.jsonl 中删除会话
        <code>{deleteCursor.session_id}</code> 的全部
        {deleteCursor.event_count} 条审计记录。
      </p>
      <p class="confirm-warning">
        请先确认 rag-demo 已停止。rag-demo 启动时会打开该文件并长期持有写入句柄，
        若它仍在运行，删除会导致其后续写入落到已被替换的文件上而丢失数据。
      </p>
      <p class="confirm-hint">
        仅删除审计日志行，不影响 agentsview 归档与 openfang.db 反馈。原文件会先备份为
        rag-audit.jsonl_bak_&lt;日期&gt;。
      </p>
      {#if deleteError}
        <p class="confirm-error">{deleteError}</p>
      {/if}
    </Modal>
  {/if}

  {#if deleteTraceTarget}
    <Modal
      title={m.rag_audit_delete_trace_title()}
      closeLabel="关闭"
      tone="danger"
      width="460px"
      onclose={closeDeleteTrace}
      footer={deleteTraceActions}
    >
      <p class="confirm-message">
        {m.rag_audit_delete_trace_message({
          trace: deleteTraceTarget.traceId,
          count: deleteTraceTarget.eventCount,
        })}
      </p>
      <p class="confirm-warning">
        {m.rag_audit_delete_trace_warning()}
      </p>
      <p class="confirm-hint">
        {m.rag_audit_delete_trace_detail({
          feedback: deleteTraceTarget.feedbackCount,
          verdict: deleteTraceTarget.hasVerdict ? 1 : 0,
        })}
      </p>
      {#if deleteTraceError}
        <p class="confirm-error">{deleteTraceError}</p>
      {/if}
    </Modal>
  {/if}
</div>

<style>
  .ag-page {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 0;
    background: var(--bg-primary);
  }

  .toolbar {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 8px 16px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
    min-height: 45px;
  }

  .title {
    display: flex;
    flex-direction: column;
    gap: 1px;
    min-width: 0;
  }

  .title h1 {
    font-size: 15px;
    margin: 0;
    color: var(--text-primary);
  }

  .subtitle {
    font-size: 11px;
    color: var(--text-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .segment {
    display: inline-flex;
    gap: 2px;
    padding: 2px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: 6px;
    margin-left: auto;
    flex-shrink: 0;
  }

  .segment button {
    border: none;
    background: transparent;
    color: var(--text-muted);
    font-size: 12px;
    padding: 4px 10px;
    border-radius: 5px;
    cursor: pointer;
    white-space: nowrap;
  }

  .segment button:hover {
    color: var(--text-primary);
  }

  .segment button.active {
    background: var(--bg-surface);
    color: var(--text-primary);
    box-shadow: 0 1px 2px var(--shadow, rgba(0, 0, 0, 0.1));
  }

  .layout {
    flex: 1;
    min-height: 0;
    display: grid;
    grid-template-columns: 320px 1fr;
    overflow: hidden;
  }

  .session-pane {
    display: flex;
    flex-direction: column;
    min-height: 0;
    border-right: 1px solid var(--border-muted);
    background: var(--bg-surface);
  }

  .pane-head {
    padding: 8px 12px;
    font-size: 11px;
    font-weight: 700;
    letter-spacing: 0.04em;
    text-transform: uppercase;
    color: var(--text-muted);
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
  }

  .session-list {
    list-style: none;
    margin: 0;
    padding: 6px;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .session-row {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    text-align: left;
    padding: 8px 8px;
    border-radius: 6px;
    border: 1px solid transparent;
    background: transparent;
    cursor: pointer;
  }

  .session-row:hover {
    background: var(--bg-surface-hover);
  }

  .session-row.active {
    background: color-mix(in srgb, var(--accent-blue) 10%, var(--bg-surface));
    border-color: color-mix(in srgb, var(--accent-blue) 30%, var(--border-muted));
  }

  .row-main {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .row-id {
    font-family: var(--font-mono, monospace);
    font-size: 11px;
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .row-meta {
    font-size: 11px;
    color: var(--text-muted);
  }

  .row-open {
    flex-shrink: 0;
    font-size: 11px;
    color: var(--accent-blue);
    text-decoration: none;
    padding: 2px 6px;
    border-radius: 4px;
  }

  .row-open:hover {
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
  }

  .row-delete {
    flex-shrink: 0;
    font-size: 11px;
    color: var(--text-muted);
    padding: 2px 6px;
    border-radius: 4px;
    cursor: pointer;
  }

  .row-delete:hover {
    color: var(--accent-red, #e55);
    background: color-mix(in srgb, var(--accent-red, #e55) 12%, transparent);
  }

  .confirm-actions {
    display: contents;
  }

  .confirm-message {
    font-size: 13px;
    color: var(--text-primary);
    margin: 0 0 6px;
    line-height: 1.5;
  }

  .confirm-message code {
    font-family: var(--font-mono, monospace);
    font-size: 12px;
    word-break: break-all;
  }

  .confirm-hint {
    font-size: 12px;
    color: var(--text-muted);
    margin: 0;
    line-height: 1.5;
  }

  .confirm-warning {
    font-size: 12px;
    color: var(--accent-red, #e55);
    margin: 0 0 8px;
    line-height: 1.5;
  }

  .confirm-error {
    font-size: 12px;
    color: var(--accent-red, #e55);
    margin: 10px 0 0;
    line-height: 1.5;
  }

  .timeline-pane {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .empty {
    color: var(--text-muted);
    font-size: 13px;
    padding: 24px;
    text-align: center;
  }

  .empty.error {
    color: var(--accent-red);
  }

  .muted {
    color: var(--text-muted);
    font-size: 12px;
    padding: 10px 12px;
  }

  .hint {
    font-size: 11px;
    line-height: 1.5;
  }

  .error {
    color: var(--accent-red);
    font-size: 12px;
    padding: 10px 12px;
  }

  .timeline-head {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    font-size: 12px;
    color: var(--text-secondary);
    padding: 0 4px;
  }

  .zero-summary {
    color: var(--accent-red);
    font-weight: 700;
  }

  .pending-summary {
    color: var(--accent-amber);
    font-weight: 700;
  }

  .review-loading {
    color: var(--text-muted);
  }

  .review-error {
    color: var(--accent-red);
  }

  .timeline {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
  }

  .trace-group {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  /* 滚动定位的落点锚：不可见，仅用于 scrollIntoView。 */
  .trace-anchor {
    height: 0;
    overflow: hidden;
  }

  /* 从归因报告跳过来时短暂高亮，指明落在哪一组。 */
  .trace-group.focused .trace-head {
    box-shadow: inset 0 0 0 2px var(--accent-blue);
    border-radius: 6px;
  }

  .trace-head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px 10px;
    padding: 6px 8px;
    margin-top: 8px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: 8px;
  }

  .trace-badge {
    font-family: var(--font-mono, monospace);
    font-size: 11px;
    font-weight: 700;
    color: var(--accent-blue);
  }

  .trace-range {
    font-family: var(--font-mono, monospace);
    font-size: 11px;
    color: var(--text-muted);
  }

  .trace-count {
    font-size: 11px;
    color: var(--text-secondary);
  }

  .trace-query {
    font-size: 11px;
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 480px;
  }

  .trace-events {
    list-style: none;
    margin: 0;
    padding: 0;
  }

  .trace-user {
    font-size: 13px;
    color: var(--text-primary);
    line-height: 1.5;
    white-space: pre-wrap;
    word-break: break-word;
    padding: 6px 8px;
    margin: 2px 0 6px;
    background: color-mix(in srgb, var(--accent-blue) 6%, var(--bg-surface));
    border: 1px solid color-mix(in srgb, var(--accent-blue) 18%, var(--border-muted));
    border-radius: 6px;
  }

  .review-panel {
    display: flex;
    flex-direction: column;
    gap: 7px;
    padding: 8px;
    margin: 2px 0 6px;
    background: color-mix(in srgb, var(--accent-blue) 4%, var(--bg-surface));
    border: 1px solid color-mix(in srgb, var(--accent-blue) 20%, var(--border-muted));
    border-radius: 8px;
  }

  .review-panel.review-split {
    background: color-mix(in srgb, var(--accent-amber) 5%, var(--bg-surface));
    border-color: color-mix(in srgb, var(--accent-amber) 28%, var(--border-muted));
  }

  .review-head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px 8px;
  }

  .review-title,
  .field-label {
    font-size: 11px;
    font-weight: 700;
    color: var(--text-secondary);
    text-transform: uppercase;
  }

  .consensus-badge,
  .pending-badge,
  .verdict-badge {
    padding: 1px 6px;
    border-radius: 999px;
    font-size: 10px;
    font-weight: 700;
  }

  .consensus-badge {
    color: color-mix(in srgb, var(--accent-blue) 70%, var(--text-primary));
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
  }

  .consensus-badge.consensus-split,
  .pending-badge {
    color: color-mix(in srgb, var(--accent-amber) 68%, var(--text-primary));
    background: color-mix(in srgb, var(--accent-amber) 15%, transparent);
  }

  .verdict-badge {
    color: color-mix(in srgb, var(--accent-green, #3fb950) 68%, var(--text-primary));
    background: color-mix(in srgb, var(--accent-green, #3fb950) 14%, transparent);
  }

  .ratings {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .rating-row {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 5px 10px;
    padding: 5px 7px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: 6px;
    font-size: 11px;
  }

  .rating-row.rating-bad {
    border-color: color-mix(in srgb, var(--accent-red, #e55) 25%, var(--border-muted));
  }

  .fb-rating {
    font-weight: 700;
    color: var(--accent-green, #3fb950);
  }

  .rating-row.rating-bad .fb-rating {
    color: var(--accent-red, #e55);
  }

  .fb-reviewer {
    color: var(--text-secondary);
    font-family: var(--font-mono, monospace);
  }

  .fb-reason {
    color: var(--text-secondary);
  }

  .fb-comment {
    flex-basis: 100%;
    color: var(--text-primary);
    line-height: 1.45;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }

  .fb-time {
    color: var(--text-muted);
    font-family: var(--font-mono, monospace);
  }

  .review-help {
    margin: 0;
    color: color-mix(in srgb, var(--accent-amber) 68%, var(--text-primary));
    font-size: 12px;
    line-height: 1.45;
  }

  .verdict-result {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 5px 9px;
    padding: 5px 7px;
    font-size: 12px;
    color: var(--text-primary);
    background: color-mix(in srgb, var(--accent-green, #3fb950) 7%, var(--bg-inset));
    border: 1px solid color-mix(in srgb, var(--accent-green, #3fb950) 21%, var(--border-muted));
    border-radius: 6px;
  }

  .verdict-result-label {
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 700;
  }

  .verdict-result-rating {
    font-weight: 700;
    color: var(--accent-green, #3fb950);
  }

  .verdict-result-note {
    min-width: 0;
    color: var(--text-secondary);
    line-height: 1.5;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }

  .verdict-editor {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 7px;
  }

  .verdict-editor :global(.verdict-note) {
    flex: 1 1 240px;
    width: auto;
  }

  .verdict-actions {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-left: auto;
  }

  .verdict-error {
    flex-basis: 100%;
    margin: 0;
    color: var(--accent-red, #e55);
    font-size: 12px;
    line-height: 1.45;
  }

  .event {
    display: grid;
    grid-template-columns: 16px 1fr;
    gap: 10px;
  }

  .event-rail {
    position: relative;
    display: flex;
    justify-content: center;
  }

  .event-rail::before {
    content: "";
    position: absolute;
    top: 0;
    bottom: 0;
    left: 50%;
    width: 2px;
    transform: translateX(-50%);
    background: var(--border-muted);
  }

  .event:first-child > .event-rail::before {
    top: 12px;
  }

  .event:last-child > .event-rail::before {
    bottom: auto;
    height: 12px;
  }

  .dot {
    position: relative;
    z-index: 1;
    margin-top: 12px;
    width: 10px;
    height: 10px;
    border-radius: 50%;
    background: var(--accent-blue);
    border: 2px solid var(--bg-primary);
  }

  .dot.zero {
    background: var(--accent-red);
  }

  .dot.feedback {
    background: var(--accent-green, #3fb950);
  }

  .card {
    margin: 8px 0;
    padding: 10px 12px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: 8px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .trace-actions {
    display: inline-flex;
    margin-left: auto;
  }

  .card.zero {
    border-color: color-mix(in srgb, var(--accent-red) 45%, var(--border-muted));
  }

  .card.feedback {
    border-color: color-mix(in srgb, var(--accent-green, #3fb950) 40%, var(--border-muted));
  }

  .card-head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
  }

  .ts {
    font-family: var(--font-mono, monospace);
    font-size: 11px;
    color: var(--text-muted);
  }

  .badge {
    display: inline-flex;
    align-items: center;
    height: 18px;
    padding: 0 6px;
    border-radius: 3px;
    border: 1px solid var(--border-muted);
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.03em;
    text-transform: uppercase;
    color: var(--text-secondary);
  }

  .badge.op-search {
    color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 9%, var(--bg-surface));
    border-color: color-mix(in srgb, var(--accent-blue) 22%, var(--border-muted));
  }

  .badge.op-feedback {
    color: var(--accent-green, #3fb950);
    background: color-mix(in srgb, var(--accent-green, #3fb950) 9%, var(--bg-surface));
    border-color: color-mix(in srgb, var(--accent-green, #3fb950) 22%, var(--border-muted));
  }

  .badge.origin {
    color: var(--text-muted);
  }

  .badge.tool {
    color: var(--text-secondary);
  }

  .badge.zero-badge {
    color: var(--accent-red);
    background: color-mix(in srgb, var(--accent-red) 12%, var(--bg-surface));
    border-color: color-mix(in srgb, var(--accent-red) 35%, var(--border-muted));
  }

  .query {
    font-size: 13px;
    color: var(--text-primary);
    line-height: 1.45;
  }

  .meta {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }

  .chip {
    font-size: 11px;
    color: var(--text-secondary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: 4px;
    padding: 1px 6px;
  }

  .chip.note {
    color: var(--text-primary);
  }

  .hits {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .hit {
    display: grid;
    grid-template-columns: 60px minmax(0, 1fr) auto;
    gap: 8px;
    align-items: baseline;
    font-size: 12px;
  }

  .hit-score {
    font-family: var(--font-mono, monospace);
    font-size: 11px;
    color: var(--text-secondary);
    text-align: right;
  }

  .hit-doc {
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .hit-chunk {
    font-family: var(--font-mono, monospace);
    font-size: 11px;
    color: var(--text-muted);
    max-width: 140px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .adopted-icon {
    color: var(--accent-green, #3fb950);
    font-weight: 700;
    text-align: center;
  }

  .hit.accepted {
    background: color-mix(in srgb, var(--accent-green, #3fb950) 12%, transparent);
    border-radius: 4px;
    padding: 2px 4px;
  }

  .hit.accepted .hit-doc,
  .hit.accepted .hit-chunk,
  .hit.accepted .hit-score {
    color: var(--accent-green, #3fb950);
  }

  .fb-note {
    font-size: 12px;
    color: var(--text-primary);
    line-height: 1.5;
    white-space: pre-wrap;
    background: var(--bg-inset);
    border-radius: 4px;
    padding: 6px 8px;
  }

  .docs-layout {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .docs-layout .empty {
    padding: 24px;
  }

  .docs-head {
    display: flex;
    flex-wrap: wrap;
    gap: 8px 16px;
    font-size: 12px;
    color: var(--text-secondary);
    padding: 0 4px;
  }

  .docs-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 12px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: 8px;
  }

  .docs-table th,
  .docs-table td {
    text-align: left;
    padding: 7px 10px;
    border-bottom: 1px solid var(--border-muted);
    vertical-align: top;
  }

  .docs-table thead th {
    font-size: 11px;
    font-weight: 700;
    letter-spacing: 0.03em;
    text-transform: uppercase;
    color: var(--text-muted);
    background: var(--bg-inset);
    position: sticky;
    top: 0;
    white-space: nowrap;
  }

  .docs-table th.sortable button {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    border: none;
    background: transparent;
    padding: 0;
    font: inherit;
    font-weight: inherit;
    letter-spacing: inherit;
    text-transform: inherit;
    color: inherit;
    cursor: pointer;
  }

  .docs-table th.sortable button:hover {
    color: var(--text-primary);
  }

  .docs-table th.sortable button.active {
    color: var(--accent-blue);
  }

  .sort-arrow {
    font-size: 9px;
    line-height: 1;
    color: var(--accent-blue);
  }

  .docs-table tbody tr:last-child td {
    border-bottom: none;
  }

  .docs-table tbody tr:hover {
    background: var(--bg-surface-hover);
  }

  .docs-table .num {
    font-family: var(--font-mono, monospace);
    color: var(--text-primary);
    white-space: nowrap;
  }

  .docs-table .ratio-empty {
    color: var(--text-muted);
  }

  .docs-table .ts-cell {
    font-family: var(--font-mono, monospace);
    font-size: 11px;
    color: var(--text-muted);
    white-space: nowrap;
  }

  .doc-cell {
    max-width: 0;
    width: 40%;
  }

  .doc-kb {
    display: block;
    font-size: 11px;
    color: var(--text-secondary);
  }

  .doc-path {
    display: block;
    font-family: var(--font-mono, monospace);
    font-size: 11px;
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .queries {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }

  .query-chip {
    font-size: 11px;
    color: var(--text-secondary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: 4px;
    padding: 1px 6px;
    white-space: nowrap;
    max-width: 180px;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  @media (max-width: 900px) {
    .layout {
      grid-template-columns: 1fr;
      grid-template-rows: auto 1fr;
      overflow-y: auto;
    }

    .session-pane {
      border-right: none;
      border-bottom: 1px solid var(--border-muted);
    }

    .session-list {
      max-height: 220px;
    }
  }

  /* ---- 归因报告 ---- */

  .tab-badge {
    margin-left: 6px;
    padding: 0 5px;
    border-radius: 8px;
    background: var(--accent-amber);
    color: var(--bg-primary);
    font-size: 10px;
    line-height: 15px;
    font-weight: 600;
  }

  .report-layout {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .report-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 16px;
    flex-wrap: wrap;
  }

  .report-head-main h2 {
    margin: 0 0 4px;
    font-size: 15px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .report-head-main p {
    margin: 0;
    font-size: 12px;
    max-width: 60ch;
  }

  .report-source {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin-top: 6px !important;
  }

  .report-source .stale {
    color: var(--accent-amber);
  }

  .report-head-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }

  .report-empty-action {
    display: flex;
    justify-content: center;
    margin-top: 12px;
  }

  .report-warn {
    padding: 8px 10px;
    border-radius: 6px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    font-size: 12px;
    color: var(--text-secondary);
  }

  .report-warn.error {
    border-color: var(--accent-red);
    color: var(--accent-red);
  }

  .report-progress {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 10px;
    border-radius: 6px;
    background: var(--bg-inset);
    font-size: 12px;
    color: var(--text-secondary);
  }

  .spinner-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--accent-blue);
    animation: report-pulse 1.2s ease-in-out infinite;
  }

  @keyframes report-pulse {
    0%,
    100% {
      opacity: 1;
    }
    50% {
      opacity: 0.25;
    }
  }

  .run-tail {
    margin: 8px 0 0;
    padding: 8px;
    max-height: 180px;
    overflow: auto;
    background: var(--bg-primary);
    border-radius: 4px;
    font-size: 11px;
    white-space: pre-wrap;
    color: var(--text-secondary);
  }

  .kpi-row {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
    gap: var(--space-3);
  }

  .kpi {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 12px 14px;
    text-align: left;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: 8px;
    cursor: pointer;
    transition: background 0.12s ease;
  }

  .kpi:hover {
    background: var(--bg-surface-hover);
  }

  .kpi.active {
    border-color: var(--accent-blue);
  }

  .kpi-num {
    font-size: 26px;
    font-weight: 600;
    line-height: 1.1;
    color: var(--text-primary);
  }

  .kpi-danger .kpi-num {
    color: var(--accent-red);
  }

  .kpi-warning .kpi-num {
    color: var(--accent-amber);
  }

  .kpi-label {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .kpi-hint {
    font-size: 11px;
    color: var(--text-muted);
  }

  .report-items {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .report-item {
    padding: 10px 12px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: 8px;
  }

  .item-head {
    display: flex;
    align-items: baseline;
    gap: 8px;
    flex-wrap: wrap;
  }

  .item-bucket {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .item-query {
    font-size: 12px;
    color: var(--text-secondary);
  }

  .item-evidence {
    margin: 6px 0 0;
    font-size: 12px;
    line-height: 1.5;
    color: var(--text-secondary);
  }

  .item-foot {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    margin-top: 8px;
  }

  .bucket-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    flex: none;
    align-self: center;
  }

  .bucket-coverage {
    background: var(--accent-red);
  }

  .bucket-retrieval {
    background: var(--accent-amber);
  }

  .bucket-env,
  .bucket-pending {
    background: var(--text-muted);
  }

  .chip.warn {
    color: var(--accent-amber);
    border-color: var(--accent-amber);
  }

  .link-btn {
    background: none;
    border: none;
    padding: 0;
    font-size: 12px;
    color: var(--accent-blue);
    cursor: pointer;
  }

  .link-btn:hover {
    text-decoration: underline;
  }

  .mono {
    font-family: monospace;
    font-size: 11px;
    word-break: break-all;
  }
</style>
