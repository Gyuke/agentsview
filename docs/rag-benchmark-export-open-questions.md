# RAG Benchmark 导出：待与上游对齐的问题

> 2026-09-20：本文已被 `docs/rag-benchmark-export.md` 取代，仅保留为
> 对齐过程记录；当前实现契约以主文档为准。

> 供 agentsview ↔ rag-demo 双方对齐。前半部分是**已从代码中查证的事实**，
> 后半部分是需要讨论决策的开放问题。

## 0. 先明确一个前提：当前上游并不消费 agentsview 的导出

这是所有问题的根源，先说清楚，避免后面讨论建立在错误假设上。

查证 `rag-demo/benchmark/rag_bench.py`（当前版本）的结果：

| 事实 | 证据 |
|---|---|
| harness **从未读取** `queries.yaml` | 全文件仅一处 `open(QUERIES, 'w')`（写），无读路径 |
| harness 直接读 `openfang.db` 取反馈 | `load_feedback()`：`sqlite3.connect(DB)` + 查 `answer_feedback` |
| harness 直接读审计 JSONL 取检索轮次 | `load_audit()`：逐行 `json.loads` 读 `rag-audit.jsonl` |
| harness 自己也会导出 `queries.yaml` | `--export` → `export_queries()` |
| harness 归因只依赖 `rounds` + `last_query` | `attribute(rounds, q)`，`q = qs[-1]` |

**结论：** agentsview 侧新增的 `user_question`、`final_answer`、`ratings[]`、
`consensus`、`effective_rating`、`candidate_pool` 六个字段，**上游当前一个都没用**。

所以文档 `docs/rag-benchmark-export.md` §6 验收标准第 1 条
（"返回的 YAML 能被 `rag_bench.py` 直接读取（字段名一致）"）**当前无法成立**——
不是字段名不一致，而是**上游没有 YAML 读取路径**。agentsview 侧已完成，
缺的是上游那一半。**这是首先要对齐的事。**

### 0.1 已查证的、不需要再讨论的问题

Codex 提出的若干疑问，从上游代码里已能直接得到答案：

| 疑问 | 上游现状（已查证） |
|---|---|
| `needs_improvement` 是否算失败样本？ | **算。** `--ratings` 默认值就是 `incorrect,needs_improvement`，注释写明"needs_improvement 是『答得不够好』，往往是覆盖缺口的另一侧面，不该被跳过" |
| 未裁决的 split 是否进入归因？ | **不进入。** 默认跳过，打印"等待人工裁决后重跑"，可用 `--include-split` 强制纳入 |
| `--rating` 过滤对 split 怎么算？ | **任一评审人匹配即算命中**：`hit = bool(wanted & set(ratings))`。与 agentsview 的 `ratingMatches` 语义一致 |
| 上游怎么判定 consensus？ | 已有 `consensus_of()`：`<=1 → single`、全同 → `agreed`、否则 `split`。与 agentsview 的 `consensusOf` **逻辑等价**（但双方各有一份实现） |

---

## 1. 需要讨论决策的问题

### Q1. `queries.yaml` 的读取契约怎么定？（最高优先级）

既然上游不读，有三条路：

- **A. 上游新增 YAML 读取**，读 agentsview 导出的文件。
- **B. agentsview 改为输出上游现有字段**（`rating` 斜杠串、`last_query`），让上游几乎不用改。
- **C. 明确废弃**，双方都直接从 `openfang.db` + `rag-audit.jsonl` 取数，agentsview 的导出仅作人工复核用。

需要拍板：**agentsview 的导出到底承担什么角色？** 是数据入口，还是人工复核产物？

**注意一个关键矛盾：** 上游是从 `openfang.db` 直接取 `rating` 的，
它**看不到** agentsview 归档库里的 `effective_rating`（人工裁决）。
所以只要走 C（或上游继续直连 openfang.db），**人工裁决对归因结果不起任何作用**——
这直接关系到 Q2。

### Q2. 人工裁决是否必须写入 `openfang.db`？

现状：评审反馈在 `openfang.db`，人工裁决在 agentsview 归档库，只在**导出时合并**。

- 好处：不动上游库、无跨库写冲突、职责清晰。
- 代价：**上游若继续直连 `openfang.db`，就永远看不到人工裁决**。裁决形同虚设。

这在 Q1 选 A/B 时不是问题，选 C 时是致命问题。
需要上游明确：**归因的输入评级以哪一方为准？**

> agentsview 侧倾向：维持现状（裁决留 agentsview），但**要求上游改为消费
> agentsview 的导出**（即 Q1 选 A 或 B），否则裁决无法生效。

### Q3. 关联答案的近似方式，误差容忍度是多少？

现状：`session + trace 时间窗 + 下一条用户文本轮次` 的近似对齐。
`session_messages` 无 `trace_id`，无法精确 JOIN。

已知误差来源：同一 session 连续快速提问时可能串轮。
实测中已发现同一 session 内存在**亚秒级**的轮次间隔（工具循环内部），
现有实现已通过"排除非 text part"和"以真实用户轮次为边界"来规避，
但**极端密集提问下仍可能取错**。

需要上游明确：
- 是否接受近似？容忍度是多少（允许取到相邻轮次的答案吗）？
- 或者：**是否可以在 OpenFang 侧给 `session_messages` 增加 `trace_id` 列**？
  这是一劳永逸的修法，比在 agentsview 侧做启发式对齐可靠得多。

### Q4. 找不到 `final_answer` 时，该导出还是跳过？

现状：仍然导出该 case，仅省略 `final_answer` 字段（`omitempty`）。

- 支持导出：case 的 `rounds`/`all_queries`/`hit_doc_paths` 仍有价值，
  归因不依赖 `final_answer`（上游是重新调 MCP 复现的）。
- 支持跳过：既然是"闭环评估"，缺回答的样本不完整。

需要上游明确。若选择跳过，agentsview 侧应改为**默认排除**，
并可加 `?include_missing_answer=true` 显式纳入。

另：建议同时给 `user_question` 和 `final_answer` 各加一个显式的
`question_missing` / `answer_missing` 标记，避免 `null` 与"字段缺失"不可区分。

### Q5. 人工裁决的范围是否应收紧为「只能裁决 split」？

现状：裁决可覆盖 `single`、`agreed`、`split` 三种；待裁决队列只列未裁决的 `split`。

- 放宽的好处：允许人工推翻单人/一致的判定（比如发现评审人判错了）。
- 放宽的风险：`effective_rating` 与 `ratings[]` 可能明显矛盾，审计时难解释。
- 收紧的理由：`single`/`agreed` 本无分歧，裁决语义不清晰。

需要上游明确：**归因时是否信任人工对 single/agreed 的覆盖？**

### Q6. `rating` / `last_query` 兼容字段何时移除？

现状：agentsview 导出**同时**输出 `effective_rating` 和 `rating`（两者当前值相同），
以及 `all_queries` 和 `last_query`（`last_query` 是 `all_queries` 的最后一个）。
这是为兼容上游现有字段名而加的冗余。

需要约定：
- 上游最终以哪个为准？
- 冗余字段的**移除时间点**（避免长期双写导致语义漂移——两个字段一旦不再同义，
  消费方会拿到矛盾数据）。

### Q7. `candidate_pool` 上游什么时候用起来？

`rag-demo` 侧已实现该字段（`AuditResponse.candidate_pool`），
agentsview 侧也已导出。但上游 `rag_bench.py` **目前仍未使用**它
（仍在用 LLM 推断 + 目录列表做归因）。

需要上游明确：**何时切换到用 `candidate_pool` 判定「召回失败 vs 排序失败」？**
在此之前，这个字段是死代码。

---

## 2. agentsview 侧建议的对齐动作（可一并确认）

1. **Q1 选 A 或 B** —— 否则本次导出无实际价值。
2. **Q3 优先考虑在 OpenFang 侧加 `trace_id`**，替代启发式时间窗对齐。
3. **Q4 若要求跳过缺回答的 case**，agentsview 侧改为默认排除 + 显式开关。
4. **Q6 约定一个移除时间点**，避免兼容字段长期双写。
5. 补充 `question_missing` / `answer_missing` 标记字段。

## 3. 附：agentsview 导出的实际字段（供上游对照）

```yaml
queries:
  - trace_id: <trace-uuid>                            # 审计 trace
    session: <session-uuid>                           # 会话 uuid
    ratings:                                          # 全部评审记录，不压扁
      - reviewer: ou_<reviewer-id-1>
        rating: incorrect
        created_at: "2026-09-11T03:50:23Z"
      - reviewer: ou_<reviewer-id-2>
        rating: helpful
        created_at: "2026-09-17T07:20:39Z"
    consensus: split                                  # single | agreed | split
    effective_rating: null                            # 人工裁决后写入；split 未裁决为 null
    rating: null                                      # 兼容字段（当前与 effective_rating 同值）
    rounds: 6                                         # search 事件数
    user_question: "<来自飞书渠道的用户提问>"           # 飞书来源的真实提问
    final_answer: "<该轮 agent 的最终回答，可截断>"      # 该轮 agent 的最终回答
    all_queries: [ ... ]                              # 本轮全部检索 query
    last_query: "…"                                   # 兼容字段 = all_queries 末项
    hit_doc_paths: [ ... ]                            # 本轮命中的文档
    candidate_pool: [ ... ]                           # rerank 前候选池 chunk_id
```

过滤参数：
- `rating`（`helpful|incorrect|needs_improvement|all`，默认 all）
- `consensus`（`single|agreed|split|all`，默认 all）
- `session`、`include_answer`（默认 true）、`answer_max_bytes`（默认 4096，超长加 `…[truncated]`）

裁决接口（写入 agentsview 归档，非 openfang.db）：
- `PUT /api/v1/rag-audit/benchmark-verdict`
- `DELETE /api/v1/rag-audit/benchmark-verdict?trace_id=...`
