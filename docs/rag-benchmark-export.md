# RAG Benchmark 数据导出

> 本文件是给 agentsview 侧的实现计划。上游需求方是失败归因 harness 的
> `benchmark/rag_bench.py`。
>
> **2026-09-20 更新**：已拍板 8 个待定问题（Q1–Q8），见 §0。实现前请先读该节。

## 0. 已拍板的决策（2026-09-20）

| # | 问题 | 决定 |
|---|---|---|
| Q1 | `queries.yaml` 读取契约 | **上游已改为读 yaml**（2026-09-20 完成并验证，见 §6.1）。agentsview 按 §3.5 格式产出即可 |
| Q2 | 人工裁决写回哪里 | **不写 openfang.db**，存 agentsview 自己的库，保持「原始反馈」与「人工裁决」分层。显式 trace 清理是唯一会删除 openfang 原始反馈的路径 |
| Q3 | 时间窗近似 vs 精确对齐 | **建议给 openfang 的 `session_messages` 加 `trace_id` 列**（根治）。该改动属 openfang 项目，非 agentsview/rag-demo。未落地前用 §3.2 的时间窗启发式 |
| Q4 | 数据缺失时 | **两类门槛不同**（复议后）：`user_question` 是**硬门槛**（缺失不导出）；`final_answer` 是**软门槛**（导出 + 打状态）。见 §3.6 |
| Q5 | 裁决范围与理由 | **允许改判非 split 的 case**。**仲裁 `split` 不必填理由**（选边本身即结论）；**改判 `single`/`agreed` 建议填**（推翻已有标注需可追溯）。不记录无来源的伪 reviewer 身份，以 `updated_at` 作为追溯依据。见 §3.4 |
| Q6 | 旧兼容字段（`rating` / `last_query`） | **不做长期双写**。删除触发条件：上游已接入新字段**且已发布**。见 §3.5.1 |
| Q7 | `candidate_pool` 为何没用起来 | 旧日志（277 条）无此字段，只有新检索才带。上游已写读取逻辑，**待新数据验证**。属过渡期死代码，非设计失误 |
| Q8 | 裁决表主键 | **仅用 `trace_id`**（实测 44/44 唯一，无跨 session 重名）。**迁移触发条件**：若 trace_id 不再全局唯一，改用 `(trace_id, session_id)`。见 §3.4 |

## 1. 背景与目标

上游侧在建一个**失败归因 harness**：对人工标注为 `incorrect` 的会话轮次，
判断这次失败出在哪一环，从而回答「RAG 检索本身有没有问题」。

归因分四类：

| 归因 | 含义 | 对结论的意义 |
|---|---|---|
| 环境故障 | 检索全部报错（collection 未建等） | 与检索质量无关，需剔除 |
| **覆盖缺口** | 知识库里根本没有能回答的资料 | **文档问题，不是检索问题** |
| 召回失败 | 库里有资料，但没进检索结果 | 检索问题 |
| 排序失败 | 资料进了结果，但没排进最终返回 | 排序/rerank 问题 |

harness 已经跑通，当前瓶颈是**数据入口**：
每个 case 需要的输入横跨两个数据源，且其中一块目前没有导出路径。

## 2. 为什么由 agentsview 做导出

agentsview **已经完成了最难的跨库关联**：

| 已有能力 | 位置（本仓库） |
|---|---|
| 读 `rag-audit.jsonl` | `internal/server/huma_routes_ragaudit.go` `readRagAuditLines` |
| 读 `answer_feedback`（openfang.db） | `humaListRagAuditFeedback` |
| 按 session 聚合 | `humaListRagAuditSessions` |
| trace 级关联检索链路 | `ragAuditSession` / `ragAuditEvent` |
| 删除单 session 审计 | `humaDeleteRagAuditSession` |

上游只缺一段序列化，而关联逻辑在这里已经存在且经过 UI 验证。
另有一个必须由 agentsview 提供的字段见 §3.2。

## 3. 需要导出的数据

### 3.1 每个 case 的字段清单

| 字段 | 来源 | 现状 |
|---|---|---|
| `trace_id` | `answer_feedback_targets.trace_id` | 已有 |
| `session` | `answer_feedback_targets.session_id` | 已有（导出字段名保持 `session`） |
| `ratings[]`（含评审人） | `answer_feedback` | 需按 §3.4 处理多人分歧 |
| `all_queries[]` | audit `operation=search` 的 `request.query` | 已有 |
| `rounds` | 同上，条数 | 已有 |
| `hit_doc_paths[]` | audit `response.hits[].chunk.doc_path` | 已有 |
| `candidate_pool[]` | audit `response.candidate_pool`（精排前 chunk_id） | 已有 |
| **`final_answer`** | **openfang session 消息（agent 最终回答）** | **缺失**，见 §3.2 |
| **`final_answer_status`** | 取数结果标记 | 见 §3.6 |
| **`alignment`** | 对齐方式（`exact` / `time_window`） | 见 §3.6 |
| **`user_question`** | **openfang session 消息（用户原始提问）** | **缺失**，见 §3.3 |

> **不导出 `user_question_status`**：`user_question` 是硬门槛，缺失即整条不导出
> （见 §3.6），该字段会恒为 `found`，没有存在价值。

**纳入范围（重要）**：只考虑
`answer_feedback_targets.channel = 'feishu'`、至少有一条评审反馈，且能关联到
`[From: ou_...]` 用户提问的轮次。只有飞书 channel 的 answer 带用户反馈
交互卡片，反馈才会落到 `openfang.db`。其他 channel 或匹配不到原始提问的
记录可以继续在审计页展示，但不进入闭环评估数据。

### 3.2 缺口一：agent 最终回答（取数规则比想象中复杂）

**为什么需要**：`rag-audit.jsonl` 只记录 RAG 工具调用，**不含 agent 的最终
自然语言回答**。而「LLM 没用好」这一类归因需要看回答——即检索结果是对的，
但 agent 没用对（例如把「文档未给出明确上限」表述成了「不支持」）。

**注意：取数有 3 个坑，全部已在真实数据上验证过：**

**坑 1：`session_messages` 里没有 `trace_id`，无法精确 JOIN。**
该表字段只有 `message_id / session_id / role / timestamp / turn_index`。
只能按 session + 时间窗近似对齐。

审计侧关联同样必须同时校验 `trace_id` 和 `session_id`，不能只按
`trace_id` 建索引；否则同名 trace 会让另一个 session 的查询、命中文档和
时间窗污染本轮样本。

**坑 2：一轮 agent loop 会产生大量 user/assistant 消息对，它们不是真实轮次。**
`session_messages` 记的是**每一次 LLM 调用**，不是「每一轮对话」。
实测 trace `trace-late-answer`（窗口 52 秒）内有 13 组 user/assistant 对，其中
`role=user` 的消息 `parts` 全是 `tool_result`（内容就是 RAG 返回的
「检索完成…请调用 rag_feedback」模板）。

**坑 3：时间窗尾部必须放宽，否则取不到答案。**
检索完成后 agent 还要：消化结果 → 调 `rag_feedback` → **最后才生成回答**。
实测该 trace 的最终回答在窗口末尾**之后 6 秒**（08:09:59 → 08:10:05）。

**正确的取数判据：**

```
最终回答 = role=assistant
           且 parts 只含 part_type='text'     ← 排除 tool_use / thinking
           且取时间上最后一条
           且搜索范围 = trace 窗口 + 尾部放宽
```

**尾部放宽的正确做法**：优先找到窗口结束后的**第一条用户文本轮次**
（`role=user` 且含 `text` part）作为硬边界，取它之前的最后一条合格
assistant 消息；该边界跨 channel，非飞书轮次只用于截断，绝不作为
`user_question` 导出。若后面没有可用的用户轮次，才使用固定 60 秒兜底
窗口。`user_question` 本身仍只认 `[From: ou_...]` 提问。

**反例（错误判据）**：

| 错误判据 | 后果 |
|---|---|
| 只按 role 取最后一条 assistant | 可能取到含 `tool_use` 的中间态（例如「我正在调 rag_feedback」） |
| 不排除 `thinking` part | 思考内容被当成回答，归因完全跑偏 |
| 时间窗严格截在 trace 末尾 | **必然漏掉最终回答**（它在末尾之后） |

### 3.3 缺口二：用户原始提问（取错比取不到更危险）

**注意：`role=user` 混了两类完全不同的东西：**

| 类别 | parts 类型 | 内容示例 |
|---|---|---|
| **真实用户提问** | `text` | `[From: ou_user_a] 设备载荷上限是多少？` |
| **工具结果回灌** | `tool_result` | `检索完成。若你的最终回答将引用本次检索结果…` |

**这不是 bug，是 Anthropic Messages API 的协议要求。** 在 Messages API 里，
一次工具调用必须构成完整一问一答：

```
assistant: [tool_use]      ← 模型说"我要调这个工具"
user:      [tool_result]   ← 结果回灌（注意：必须是 user 角色）
```

从模型视角看，「工具返回了什么」和「用户说了什么」是同一类东西——都是
**外部输入给模型的信息**。模型的角色只有两个：自己说的（assistant）和
别人给的（user）。所以 `tool_result` 走 user 通道是刻意的设计，
OpenFang 只是忠实存储。

**样本数据**（session `session-sample-a`）：

```
role=user      的 part_type: text=30, tool_result=108, image=2
role=assistant 的 part_type: thinking=35, text=84, tool_use=108
```

注意 `tool_use` 与 `tool_result` 数量完全相等（108=108）——**这是协议的结构性
要求，不是巧合**。也说明该 session 里 agent 调了 108 次工具。

**注意：** 若不过滤 part_type，`user_question` 会取到 RAG 返回的
「检索完成」模板。**这比取不到值危险得多**——它看起来像一句话，
实际是工具结果，会污染整个归因链路。

**正确判据：**

```
真实用户提问 = role=user
               且某个 part 的 type='text'
               且该 part 内容形如 [From: ou_...]（feishu 消息来源标识）
```

去掉 `[From: ou_...]` 前缀后，问题文本还必须非空。其他 `text` part 可能来自
其他 channel，不作为本轮 `user_question`，也不进入导出。

若 trace 开始前同一 session 的**最近一条真实用户文本轮次**不是
`[From: ou_...]`，不能继续向前回溯并复用更早的飞书提问；该 trace 不属于
可闭环评估数据。

### 3.4 反馈冲突：多人评审同一 answer

**背景**：`answer_feedback` 的唯一约束是 `UNIQUE(answer_id, reviewer_open_id)`,
即**「同一 answer + 同一评审人」只能有一条**。实测重复反馈全部来自
**不同评审人**（同一人改判会被 UPDATE 覆盖，不会留下多条记录）。

**样本案例**（`trace-disagreement-a`）：

```
评审人A (ou_reviewer_a)  incorrect  2026-09-08
评审人B (ou_reviewer_b)  helpful    2026-09-17
```

样本中存在多名评审人，且同一评审人会覆盖多轮。

**处理原则：导出只做快照，不做取舍。**

理由：重复 = 分歧（不可能是同一人改主意，schema 已保证）。而分歧本身是
**有价值的信号**——说明该 case 是边界情况。若导出时压成一个值（无论取最新
还是取最严），都等于**替人做了决定**，且 UI 上「重新裁决」就失去了依据。

**注意：为什么不能取「最严」** 若把 `trace-disagreement-a` 压成 `incorrect`，但实际它
是 helpful，归因引擎会去找「为什么失败」——**而它根本没失败**，于是造出一个
**假的「覆盖缺口」样本**。用最严做归因输入等于制造假样本。

**导出格式：原样带出 + `consensus` 标记**

```yaml
- trace_id: trace-disagreement-a
  ratings:
    - reviewer: ou_reviewer_a
      rating: incorrect
    - reviewer: ou_reviewer_b
      rating: helpful
  consensus: split          # single | agreed | split
  effective_rating: null    # 待 UI 裁决后写回
```

**三分法：**

| 情况 | consensus | 处理 |
|---|---|---|
| 只有一条反馈 | `single` | 直接用它 |
| 多条且一致 | `agreed` | 用共识值 |
| 多条且矛盾 | `split` | **不预设结论**，进人工队列，等 UI 裁决 |

**`ratings[].reviewer` 字段是必需项**——它是「链路能对比出来」的技术前提。
若只导出压扁后的 `rating`，UI 就没有「重新选中」的选项可给。

**⚠️ 裁决结果不写回 openfang.db**（Q2 已拍板）。人工裁决由 agentsview
保存，`answer_feedback` 继续保持为评审人原始提交内容。只有 §3.9 的显式
trace 清理会删除 OpenFang 中对应的原始反馈。

**因此 agentsview 需自建裁决存储**，建议：

```sql
CREATE TABLE rag_bench_verdicts (
  trace_id     TEXT PRIMARY KEY,
  effective_rating TEXT NOT NULL,   -- helpful | incorrect | needs_improvement
  note         TEXT,                -- 可选理由
  updated_at   TEXT NOT NULL
);
```

导出时把 `effective_rating` 写入导出结果的 `effective_rating`。

**主键为何只用 `trace_id`（Q8 复议）**

实测数据支持单键：

| 事实 | 数据 |
|---|---|
| `answer_feedback_targets` 中 trace_id 唯一 | 44/44 |
| `rag-audit.jsonl` 中 trace_id 跨 session 重名 | 0（共 46 个 trace） |

`trace_id` 由上游 UUID 生成，当前不存在同一 trace 跨 session 的情况。

**⚠️ 迁移触发条件**：若将来 trace_id 不再全局唯一（上游改用非 UUID 生成、
或出现跨 session 复用），**必须迁移到复合主键 `(trace_id, session_id)`**，
并同步 API、UI、导出查找。UI 展示本就应该按 §3.2 校验 `trace_id + session_id`，
届时以 UI 的标识为准。

**理由是否必填（Q5 复议）** —— **一律不强制，按裁决类型区分建议强度**：

| 裁决类型 | `note` | 原因 |
|---|---|---|
| **裁决 `split`**（多人分歧） | **不必填** | 你在两方意见中选边，**选择本身就是结论**——「我认同 A」无需额外解释 |
| **改判 `single` / `agreed`** | **建议填（不强制）** | 这是**推翻已有标注**（可能是别人打的）。**未填时的实际代价**：其他人无法从数据里看出改判原因，只能看到「这个标记被改了」，**可追溯性减弱** |

> ⚠️ 措辞澄清：不填理由**不会造成「审计链路断掉」**——`updated_at` 与
> `effective_rating` 仍在记录中，链路是完整的，只是**缺少改判动机的解释**。
> 这是产品上有意接受的取舍：强制填理由会增加操作摩擦，可能阻碍正常纠错。

**裁决时间与身份**

agentsview 当前**没有明确的登录用户上下文**。因此**不记录构建不出真实语义的
`reviewer` 字段**：

- **以 `updated_at` 作为追溯依据**；
- 将来 agentsview 接入登录后，再增加真实用户字段并做显式迁移。

**裁决范围**：
- `split` 的 case —— **必须**裁决（否则无法归因）
- `single` / `agreed` 的 case —— **可选**改判（单人标注也会错）

### 3.5 建议的导出格式

与上游 `benchmark/queries.yaml` 对齐：

```yaml
schema_version: 1
exported_at: "2026-09-20T10:49:44Z"
source: agentsview
source_fingerprint:
  feedback_count: 5
  audit_count: 6
  queries_sha256: "..."
queries:
  - trace_id: trace-sample-a
    session: session-sample-a
    ratings:                                               # 见 §3.4
      - reviewer: ou_...
        rating: incorrect
        created_at: "2026-09-08T12:00:00Z"
    consensus: single                                      # single | agreed | split
    effective_rating: incorrect                            # split 未裁决时为 null
    rating: incorrect                                      # 兼容旧 queries.yaml（见 §3.5.1）
    rounds: 6
    user_question: "设备载荷上限是多少？"                     # 见 §3.3（硬门槛）
    final_answer: "..."                                     # 见 §3.2（可截断 4KB）
    final_answer_status: found                              # 见 §3.6
    alignment: time_window                                  # 见 §3.6
    all_queries:
      - "设备载荷上限是多少？"
      - "设备规格 载荷 上限"
    last_query: "设备规格 载荷 上限"                        # 兼容旧 queries.yaml（见 §3.5.1）
    hit_doc_paths:
      - "资料/设备A/规格说明"
    candidate_pool:
      - "chunk_id_1"
      - "chunk_id_2"
```

**注意**：`user_question` 缺失的 case **不在此文件中出现**（硬门槛，见 §3.6）。
上例是正常情况的完整形态。

### 3.5.1 兼容字段（`rating` / `last_query`）的迁移门槛

`rating` 和 `last_query` 是为**已发布的旧版 `benchmark/queries.yaml` 消费方**
保留的扁平兼容字段。当前**必须继续输出**，否则旧消费方立刻断掉。

**Q6 已拍板：不做长期双写**（两套语义不同——一个是压扁的结论，一个是原始
分歧，并存必然被误读）。**删除的触发条件**：

1. 上游 harness 已接入 `rating` → `effective_rating`、`last_query` → `all_queries`
2. 该上游版本**已发布**（不只是合并）

满足后删除这两个字段。**在满足前，三处（§0 决策表、本节、示例 YAML）
的表述是「当前输出、将来删除」，不矛盾。**

### 3.6 数据缺失的处理：final_answer 缺失打标记，user_question 缺失排除

**两类数据的门槛不同（Q4 复议）：**

| 字段 | 缺失时的处理 | 理由 |
|---|---|---|
| `user_question` | **硬门槛**——缺失则**只展示、不进入导出** | 不知道在问什么，归因任务本身不成立 |
| `final_answer` | **软门槛**——照常导出 + 打状态标记 | 「知识库有没有资料」的判断不依赖回答；只是「LLM 没用好」这个桶判不了 |

> 注意：**并不存在「找不到合格数据一律必须导出」这条规则**——它只适用于
> `final_answer`。`user_question` 缺失时该轮次完全不进导出。

因此本导出**不存在 `user_question_status` 字段**——既然缺失就不导出，
该字段恒为 `found`，保留它没有意义。**但 UI 上仍应展示这类"不完整"的
case**（供人工判断是否值得补数据），只是不进闭环导出。

**`final_answer_status` 枚举**

合格回答的判据是：**assistant 消息的所有 parts 都只是 `text` 类型**
（见 §3.2）。因此「不合格」在实现上有多种组合：

| 实际情况 | 是否合格 |
|---|---|
| 只有 `text` | ✅ 合格 |
| 只有 `thinking` | ❌ |
| 只有 `tool_use` | ❌ |
| `text` + `tool_use` 混合 | ❌ |
| `text` + `thinking` 混合 | ❌ |

**枚举（合并处理，不做逐组合细分）：**

| 取值 | 含义 |
|---|---|
| `found` | 找到合格回答（parts 全为 text） |
| `no_eligible_text_answer` | 窗口内有 assistant 消息，但**没有一条满足「parts 全为 text」**——涵盖只有 thinking / 只有 tool_use / 各类混合 |
| `missing_window` | 时间窗内（含尾部放宽）未找到任何 assistant 消息 |
| `not_requested` | 调用方传了 `include_answer=false`，本就不取 |

> **为何不细分 `only_tool_use` / `only_non_text`**：上游 harness 对这几种
> 情况的处理完全一致（都是「无可用回答」），**没有消费方需要区分**，
> 细分只会带来枚举维护负担，且必然漏掉新的组合（如 text + thinking 混合）。
> 若将来确有诊断需求，可在 UI 侧按 parts 类型现算，无需固化进导出契约。

**`alignment` 枚举**：

| 取值 | 含义 |
|---|---|
| `exact` | 精确对齐（未来加了 `trace_id` 列后） |
| `time_window` | 靠时间窗近似（当前实现） |

**上游 harness 的消费规则**：
- `final_answer_status != found` → 归因时**跳过**（无法判断「LLM 是否用好」），
  但**统计进报告**，让缺失可见
- `alignment == time_window` → 归因结论标注「对齐方式为近似，可信度略低」

**⚠️ 补充：`missing_feishu_question` 的处理**

`role=user` 的消息可能不是 feishu 来源（无 `[From: ou_...]` 前缀），
或不是真实提问。按上表，这属于 `user_question` 缺失 → **不导出**。
若将来需要区分「没有提问」和「有提问但格式异常」，再引入子状态；
当前不做该区分，避免枚举过度膨胀。

### 3.7 建议的接口形态

两条路，任选：

**A. 新增导出端点（推荐）**

```
GET /api/v1/rag-audit/benchmark-export?rating=incorrect
→ application/yaml 或 application/json
```

参数：
- `rating`（可选）：`helpful | incorrect | needs_improvement | all`，默认 `all`
- `session`（可选）：限定单个 session
- `consensus`（可选）：`single | agreed | split | all`，默认 `all`（见 §3.4）
- `include_answer`（可选，默认 `true`）：是否带 `final_answer`
- `answer_max_bytes`（可选，默认 4096）：回答截断长度

`rating` 过滤按 `effective_rating` 判断：单评审和评审一致的 case 使用共识值；
人工裁决过的 case 使用裁决值；未裁决的 `split` 为 `null`，不会被任何具体
`rating` 命中。`rating=all` 仍会原样导出这些分歧快照，供人工裁决。这样
harness 使用 `rating=incorrect` 或 `rating=needs_improvement` 时不会误收
尚未闭环的样本。

**B. CLI 子命令**，把上述端点包一层：

```
agentsview rag export-benchmark --rating incorrect -o queries.yaml
```

### 3.8 RAG audit 页面的归因报告

RAG audit 页面新增「归因报告」页签，直接消费 rag-demo harness 产出的
`benchmark/reports/*.json`。「生成报告」会在后台执行完整链路：

1. agentsview 重新生成最新 `benchmark/queries.yaml`；
2. 运行 `benchmark/rag_bench.py --repeat=3`；
3. 页面轮询任务状态，完成后自动加载最新报告。

标准目录布局下无需额外配置：`benchmark_script_path`、
`benchmark_project_dir` 和 `benchmark_reports_dir` 都可从 `rag_audit_path`
推导。非标准布局可使用同名 TOML 配置项或
`AGENTSVIEW_BENCHMARK_SCRIPT_PATH`、`AGENTSVIEW_BENCHMARK_PROJECT_DIR`、
`AGENTSVIEW_BENCHMARK_REPORTS_DIR` 覆盖。

对应端点：

```
GET  /api/v1/rag-audit/benchmark-reports
GET  /api/v1/rag-audit/benchmark-report?file=<name>
GET  /api/v1/rag-audit/benchmark-run
POST /api/v1/rag-audit/benchmark-run
GET  /api/v1/rag-audit/benchmark-check
```

### 3.9 Trace 级评审数据清理

审计页每个 trace 组提供「删除本轮数据」入口。它用于剔除 rag-demo 开发期间
产生的无效调用，会同时清理：

- `rag-audit.jsonl` 中匹配该 `trace_id + session_id` 的全部行；
- `openfang.db` 中对应的 `answer_feedback` 和 `answer_feedback_targets`；
- agentsview 归档库中该 trace 的人工裁决。

接口：

```
DELETE /api/v1/rag-audit/trace?trace_id=<trace>&session_id=<session>
```

`session_id` 是必需参数，避免同名 trace 误删其他会话。删除 JSONL 前会生成
`rag-audit.jsonl_bak_<日期_时间>` 备份；OpenFang 反馈删除在单个 SQLite
事务中完成。删除前必须先停止 rag-demo，避免它仍持有旧审计文件写入句柄。

## 4. 实现要点

1. **复用现有查询，不要重写关联逻辑。** `humaListRagAuditFeedback` 已有
   feedback → trace 的 join；`humaListRagAuditSessions` 已有 trace → 事件的聚合。
   导出是这两者的组合 + 序列化。

2. **注意：`final_answer` 的取数必须按 part_type 过滤**（详见 §3.2 的三个坑）：
   ```
   role=assistant 且 parts 只含 part_type='text'，取时间上最后一条
   ```
   **不能只按 role 取最后一条 assistant** —— 中间态消息（含 `tool_use`）
   会被误取，例如「我正在调 rag_feedback」不是回答。
   **也不能把 `thinking` 当答案。**

3. **注意：`user_question` 必须排除 `tool_result`**（详见 §3.3）。
   `role=user` 混了「真实提问」和「工具结果回灌」两类。取数判据：
   ```
   role=user 且某个 text part 的内容形如 [From: ou_...]
   且去掉标记后非空
   ```
   **若不过滤，会取到 RAG 返回的「检索完成」模板** —— 比取空值更危险。

4. **注意：时间窗尾部必须放宽**（详见 §3.2 坑 3）。
   `session_messages` 无 `trace_id`，只能按 session + 时间窗近似对齐。
   而**检索完成后 agent 还要消化结果、调 feedback、最后才生成回答**，
   所以最终回答**必然在 trace 窗口之后**（实测晚 6 秒）。
   **正确做法**：优先找到窗口结束后的**第一条用户文本轮次**作为硬边界，
   取它之前的最后一条合格 assistant 消息；该边界跨 channel，但只有
   `[From: ou_...]` 提问会作为 `user_question` 导出。若后面没有可用的用户
   轮次，则使用固定 60 秒兜底窗口。
   **若严格截在 trace 末尾，必然漏掉答案。**

5. **注意：反馈冲突不做取舍**（详见 §3.4）。原样带出所有 `ratings[]` +
   `consensus` 标记，`split` 的 `effective_rating` 留 `null` 等 UI 裁决。
   **`ratings[].reviewer` 是必需字段**，否则 UI 无从展示分歧。
   具体 `rating` 过滤使用 `effective_rating`，避免未裁决样本进入归因。

6. **截断策略。** `final_answer` 可能很长；默认截断到 4KB 并在字段末尾加
   `…[truncated]` 标记，避免导出文件过大。

7. **只读。** openfang.db 以 `mode=ro` 打开（现有反馈端点已是这个做法）。

8. **`user_question` 取不到时不导出该 case，不要编造。** 闭环评估必须能
   追溯到触发这一轮的 `[From: ou_...]` 用户提问；部分历史 session
   （例如 `rounds=0`）没有真实提问，因此不属于可闭环评估的数据。

## 4.5 UI 侧需配套的能力

导出只是链路的一半。要让「人工有最终确定权」成立，UI 还需要：

| 能力 | 说明 |
|---|---|
| **展示分歧** | `consensus: split` 的 case 显示「A 说 incorrect，B 说 helpful」，而非单一值 |
| **裁决录入** | 允许人工选定最终 `effective_rating`，写回作为本次快照的结论 |
| **待裁决队列** | 列出所有 `split` 且未裁决的 case，避免积压 |
| **对比视图** | 展示「原始反馈 vs 裁决结果」，使分歧可追溯（对应「保证链路能对比出来」） |

**注意：未裁决的 `split` 不应进入归因** —— harness 侧会跳过（避免用错误输入
造出假样本），等裁决后再跑。

## 5. 与 audit 新增字段的关系（协同项）

上游计划在 `rag-audit.jsonl` 的 search 记录里新增一个字段：

```json
{
  "operation": "search",
  "request": { "...": "不变" },
  "response": {
    "hits": [ "... 不变，仍是 rerank 后的 top_k ..." ],
    "candidate_pool": ["chunk_id_1", "..."]   // 新增：rerank 前的候选池
  }
}
```

**用途**：让 harness 能自动区分「召回失败」和「排序失败」——判断正确答案
是否进了精排前的候选池。当前 harness 靠 LLM 引用文档名间接推断，有
`candidate_pool` 后可直接判定。

**对 agentsview 的影响**：

| 影响面 | 结论 |
|---|---|
| 解析层 | **不受影响。** `ragAuditLine` 注释明确写「宽松结构，兼容 v1/v2 与后续 v3」，Go `json.Unmarshal` 忽略未知字段 |
| 现有 UI | **不受影响**，不读该字段就不渲染 |
| 导出 | **需同步**：若要导出候选池，需在 `ragAuditResp` 加对应字段 |
| 旧日志 | 兼容，新字段用 `Option`，旧行解析为 `None` |

**建议**：字段加在 `response` 下而非 `request` 下——它是检索的产出，
不是调用方的入参。

**当前状态**：该字段已在上游侧实现并验证通过
（`AuditResponse.candidate_pool`，
带 `skip_serializing_if`，精排关闭时不出现）。agentsview 解析层忽略旧日志
中的缺失值，导出时按 trace 聚合去重后写入 `candidate_pool[]`。

## 6. 验收标准

1. `GET /api/v1/rag-audit/benchmark-export?rating=incorrect` 返回的 YAML 字段
   带有一致的顶层元数据，并与 `benchmark/queries.yaml` 契约兼容。每个
   `queries[]` case 额外带出 `ratings[]`、
   `consensus`、`effective_rating`、`user_question`、`final_answer`、
   `final_answer_status`、`alignment` 和 `candidate_pool[]`
2. **`final_answer` 取到的是「给用户的回答」而非中间态**——逐条抽查：
   不应取到「我正在调用 rag_feedback」这类含 tool_use 的消息，
   也不应取到 thinking 内容。**取不到时按 §3.6 打状态标记，不算失败**
   （Q4 复议：只有 `user_question` 是硬门槛）
3. **`user_question` 是真实提问，不是「检索完成…」模板**——逐条抽查。
   缺失的轮次**不进入导出**（硬门槛），但应在 UI 展示
4. **`consensus: split` 的 case 原样带出全部 `ratings[]`**，
   未裁决的 case `effective_rating` 为 `null`
5. 现有 RAG audit 页面**无回归**（列表、session 视图、反馈展示均正常）
6. openfang.db 无写入（只读打开）
7. 有飞书反馈但匹配不到 `[From: ou_...]` 提问的轮次不出现在导出中；
   如果该轮仍能在审计页展示，不影响展示链路。

### 6.1 Q1 落地状态（重要）

**上游 `rag_bench.py` 已改为读 `queries.yaml`**（2026-09-20 完成并验证）。
读取优先级：

```
queries.yaml（agentsview 导出契约）
    ↓ 读不到才退回
openfang.db 直连（本地兜底，用于 1、2 完成前的过渡期）
```

因此**验收标准 1 现在等价于端到端接入**——导出 YAML 能被上游直接消费。
YAML 顶层元数据是 additive 变更；消费方应忽略未知字段，并在自身版本更新后
校验 `schema_version`。

### 6.2 ⚠️ YAML 必须带元数据（新鲜度校验）

**风险**：隐式 fallback 有一个盲区——**如果 `queries.yaml` 是个旧文件但仍然
存在，上游会静默消费过期数据，永远不会触发 fallback**，且无人察觉。

**仅靠「文件是否存在」无法判断数据是否过期。** 因此导出必须带头部元数据：

```yaml
schema_version: 1
exported_at: "2026-09-20T10:49:44Z"      # 导出时刻
source: agentsview
source_fingerprint:                       # 数据源指纹，用于判断是否已更新
  feedback_count: 5                      # 符合飞书提问硬门槛的原始反馈行数
  audit_count: 6                         # 本次读取的审计 JSONL 行数
  queries_sha256: "..."                  # 当前查询内容的 SHA-256
queries: [...]
```

**上游 harness 的消费规则**（按强度递增）：

| 条件 | 行为 |
|---|---|
| 缺 `schema_version` | **报错退出**——格式无法保证，可能是旧契约 |
| `schema_version` 不匹配 | **报错退出**——契约版本不一致 |
| 缺 `exported_at` 或格式不合法 | **警告，但继续处理** |
| `exported_at` 超过阈值（建议 7 天） | **警告，但继续处理**（不阻断，因为可能确实没有新数据） |
| 存在未知字段 | 忽略，保持向前兼容 |
| 正常 | 正常处理 |

**关键：把数据源与导出时间写进报告头部**，例如：

```
数据源：benchmark/queries.yaml（导出于 2026-09-20 10:49，3 天前）
```

**让新鲜度可见，比硬失败更实用**——你一眼就能看出报告是基于什么时候的数据
跑出来的。若发现数据陈旧，自行重新导出即可。

**为何不改成「显式传入，文件不存在就失败」**：

- 这解决不了核心问题——**过期文件同样「存在」**，硬失败照样会读到旧数据
- 且会阻断过渡期（agentsview 导出未就绪时 harness 完全不可用）
- 元数据校验温和且真正针对「静默」这一要害

## 7. 仍待确认（原 10 项中 8 项已拍板，见 §0）

- **导出格式用 YAML 还是 JSON？** 保留 YAML——便于人工增删注释，
  且上游改读 yaml 后（Q1）这条路已定。JSON 的机器处理优势在本场景用不上。
- **时间窗近似的误差容忍度（Q3 的退路）。** 若 openfang 短期不加 `trace_id`，
  现有启发式（时间窗 + 尾部放宽到下一条真实提问）是否够用？**已知风险**：
  同一 session 连续快速提问时可能取到相邻轮次的回答。
  建议先跑一批数据看 `alignment: time_window` 的样本里有多少存在
  「下一轮提问距窗口末尾 < 10 秒」——**这是判断误差可控性的量化依据**。
- **给 openfang 提 `trace_id` 改造的具体载体。** Q3 的根治方案要落到
  openfang 项目（非本仓库、非 rag-demo）。需要确定：谁提 issue / 走什么流程 /
  新增列后旧数据如何回填（`NULL` 还是按时间窗推测填充）。

## 8. 附：已在本机数据上验证的事实

以下结论均经实测，可直接作为实现依据：

| 结论 | 证据 |
|---|---|
| `session_messages` 无 `trace_id`，无法精确 JOIN | schema 仅有 `message_id/session_id/role/timestamp/turn_index` |
| `tool_result` 与 `tool_use` 数量恒等 | session `session-sample-a`：各 108 |
| `role=user` 混装真实提问与工具回灌 | 同 session：`text=30, tool_result=108, image=2` |
| 最终回答晚于 trace 窗口 | trace `trace-late-answer`：回答晚于窗口 6 秒 |
| 重复反馈均为不同评审人 | `trace-disagreement-a` 两条来自不同 `reviewer_open_id` |
| 同一人改判不会产生多条 | schema `UNIQUE(answer_id, reviewer_open_id)` |
| `trace_id` 全局唯一，不跨 session | `answer_feedback_targets` 44/44 唯一；`rag-audit.jsonl` 46 个 trace 跨 session 重名 0 条 |
