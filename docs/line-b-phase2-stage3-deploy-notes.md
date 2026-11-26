# 线 B 第二阶段第 3 棒部署说明（待复核，绝不自行安装部署）

分支 `line-b-recall`，本棒两笔提交（见汇报）。以下步骤须 Gemini 审完、
Muse 确认、用户点头后才执行；本棒只在 staging（`/tmp` + 分支内）
验证过，没有动生产。

## 1. 部署前置：主库文件

生产主库路径（约定）：`/root/super-agent-007/data/memory/summaries.db`

生成办法（在生产机上跑，简介批次到了之后）：

```bash
cd /root/super-agent-backup
# 1) 接入新批次简介（外部写好，summary-ingest 只做质检 + 发号）
go run ./cmd/summary-ingest \
  -summaries /path/to/新一批简介 \
  -memory-root /root/super-agent-007/data/memory \
  -store /root/super-agent-007/data/简介 \
  -manifest /root/super-agent-007/data/简介/manifest.json
# 2) 回填进主库（向量走本地 Ollama，不走 API）
go run ./cmd/summary-backfill \
  -manifest /root/super-agent-007/data/简介/manifest.json \
  -store /root/super-agent-007/data/简介 \
  -db /root/super-agent-007/data/memory/summaries.db \
  -memory /root/super-agent-007/data/memory
```

注意：

- `-db` 指向生产主库路径前，先确认该文件不存在或已备份；
  回填是 upsert（按 doc_id 幂等），重复跑不重号。
- pending 的简介不入库，等家长改完重提后由接入 CLI 复用 doc_id
  再入库，下次回填自动带上。
- 全量 1693 篇回填 = 77 批简介全部到了之后再跑（本棒只做了
  第一批 14 篇的 machinery 验证）。
- 备份：部署前先备份旧 `vectors.db`（生产仍有它，但两阶段
  召回不再读它）与现有 `summaries.db`（如有）。

## 2. 上线：换二进制

```bash
cd /root/super-agent-backup
git checkout line-b-recall
git pull   # 确认在审完的提交上
go build -o /tmp/super-agent-new ./cmd/super-agent
# 停服务、换二进制、重启（沿用线 A 的部署流程，先备份旧二进制）
```

守护进程启动时：`data/memory/summaries.db` 存在即自动接线
（日志里有 `summary store ready: docs=N vectors=M`），
走新两阶段召回；文件不存在则走旧 Recall 路径并记日志
（`summary store: ... 不存在，走旧 Recall 路径`）。

## 3. 开关（回滚对照）

- `internal/memory/memory.go`：
  - `TwoStageRecallEnabled`（默认 true）：改 false 即回落旧 Recall
    路径（代码一字未删），重启生效。
  - `SemanticEnabled`（默认 false）：新简介向量语义腿的总开关。
    本棒实测结论见汇报——语义腿只在 14 篇简介上有向量，
    是否开启由评测数字定，不要硬开。
- 旧 `vectors.db` 语义臂：生产已停用（`SemanticEnabled=false`），
  两阶段召回不再读它；是否删除旧库由后续决定，本棒不动。

## 4. 验证（部署后）

```bash
# 评测（staging 记忆副本 + staging 主库，先跑通再上生产路径）
go run ./cmd/recall-eval \
  -memory /tmp/mem-b-line-b \
  -eval internal/memory/testdata/golden_eval.json \
  -twostage -store /tmp/line-b-summaries.db \
  -out /tmp/baseline-report-phase2-stage3.json
```

对照门槛（v2.1）：Recall@40 ≥96%、Recall@3 ≥84%、Top-1 ≥64%、
负样本误推率 ≤10%、P95 ≤800ms、交付卡片 ≤400 tokens。
注意：X05/X06 的目标是 `l1/2026-11-24.log`，L1 只留 7 天，
12 月 1 日后失效，重跑前须更新评测集。

## 5. 本棒已知遗留（部署前须知）

- 主库目前只有第一批 14 篇简介有向量；其余 1679 篇文档没有
  简介，走全文 BM25 腿进候选池——评测数字反映的是这个状态，
  不是全量简介建完的状态。
- `superseded_by` 全库为空（无旧文档标记），降权/拉入/前缀
  只有 machinery + 单测，无真实数据验证。
- `search_memory` / `read_memory` 工具（第二阶段已定，每任务
  各最多 3 次）本棒未写，留给后续棒。
- front.doRecall 仍无账本句柄（第一阶段遗留第 6 条），
  前置路径的召回不记 recall_log；AGENT 路径（memorySummary）
  已记账。
- 巡检（ScanDir / RedactFile）未接线；用户已取消巡检原地改写，
  接线时只做只读扫描、不自动改文件。
