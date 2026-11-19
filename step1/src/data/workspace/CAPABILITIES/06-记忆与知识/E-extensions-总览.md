# 37 个 steel-will 扩展总览

> **一句话**：`/root/.pi/agent/extensions/*.ts` 共 **37 个** TypeScript 扩展，每个在 `session_start`（少数挂 `session_end`/`tool_call`）时注册工具或后台逻辑。
> **不逐个写 md**，按功能分 7 组，需要细节时直接看对应 `.ts`。

## 分组（实测 `ls *.ts | wc -l` = 37）
| 组 | 文件（数量） | 干什么 | 暴露的工具 |
|---|---|---|---|
| ① 记忆读写 | `30-wiki-engine` `32-auto-trigger` `33-lint-system` `34-temporal` `35-self-status` `long-term` `memory-skeleton` | wiki 页写入、候选捕获、健康检查、时序降权/取代 | `wiki_upsert_page` `wiki_capture_candidate` `wiki_lint` `mark_superseded` `set_memory_validity` `list_temporal` `self_status` `usage_report` `detect_conflicts` |
| ② 检索召回 | `recall-tool` `retrieval` `vector-radar` `codegraph` `sensory` `ontology` | 多模式召回、中文别名扩展、读预算、代码图谱 | `recall_memory` `route_query` `filter_results` `check_read_budget` `expand_chinese_aliases` `codegraph` |
| ③ 自治与调度 | `autonomic-nervous` `heartbeat` `scheduler` `scheduler-v2` `subconscious` `state-control` `swarm` `transcendence` | 唤醒条件、动机评分、心跳、反思反射、跨实例同步 | `check_wakeup_conditions` `evaluate_motivation` `create_background_report` `check_reflex_*` `record_reflex_event` `create_soul_manifest` `start_sync` `check_sync_status` |
| ④ 晋升与筛选 | `auto-promoter` `candidate-screener` `adjudicator` `reflection` `migration` `bootstrap` `governance` | L1→L3 自动晋升、候选打分、冲突裁决、启动自检（**0 个工具**，纯后台） | — |
| ⑤ 成就与体验 | `achievement-motivator` `experience-injector` `failure-learner` `clone-imprint` `hallucination-eliminator` | 成就加成、经验注入、失败学习、口吻印记、写入闸门 | `get_achievement_motivation_boost` `get_token_roi` `validate_memory` `check_preference_strength` `record_recall_signal` `update_access_stats` `check_recall_contract` |
| ⑥ 工厂与网关 | `forge` `gateway` `security` | 工具蓝图→沙箱→列装；路由；安全闸 | `create_blueprint` `test_in_sandbox` `enlist_tool` |
| ⑦ 采集与守护 | `l1-logger` `32-auto-trigger`(兼具) | session JSONL → L1 落盘（与 Python watcher 双轨） | — |

## 实际效果（实测）
- `grep -c 'name: "…"'` 合计 **48 处**，去掉 schema 里的 `keywords/aliases/summary/reflections/lessons/cases` 噪声，**实际可用工具约 40 个**（与本会话工具清单吻合）。
- 全部只做「注册工具 / 挂 `session_start` 钩子」，**没有常驻进程**；进程级常驻只有 `l1_watcher.py` / `refine_daily.py`（见 `L-l1-watcher.md`）。
- 组④ 7 个文件**一个工具都不暴露** —— 它们只在会话开始/结束时静默跑，出问题时**没有可直接调用的入口**。

## 怎么调
```
ls /root/.pi/agent/extensions/*.ts | wc -l          # 37
grep -l 'name: "' /root/.pi/agent/extensions/*.ts    # 哪些扩展暴露工具
grep -o 'on(.[a-z_]*' <file>.ts | sort -u            # 该扩展挂的事件钩子
```

## 坑
- **改扩展必须重启 pi 进程**才生效（`session_start` 才加载），改完不重启 = 没改。
- 状态写在 `/root/.pi/agent/memory/system/*.json`（`achievements / temporal / token_roi / usage / auto_promoter_state / refiner_state`），删了等于清零。
- 组① 与组② 有耦合：`recall_memory` 的读预算来自 `vector-radar.check_read_budget`，**向量库实测为空**
  （`system/vector_db/index.json` 的 `items: []`）→ 所谓 hybrid 实际走词法兜底（见 `R-recall-memory.md`）。
- 37 个文件全部 `4657–30174` 字节的旧时间戳（2026-10-25 15:04 同一批落盘），**看起来像模板批发**；组④ 无工具这一点值得后续核对是否真在跑。
