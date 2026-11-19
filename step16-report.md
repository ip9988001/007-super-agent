# 超级 Agent 007 第 16 步报告：二号记忆迁移进私密 007

日期：2026-11-18。目标：私密库 super-agent-007 成为带着二号全部记忆的新二号。
切流顺序（用户定）：先迁记忆、验过能用，再考虑切流；要成熟 Agent，不要空白 Agent。

## 一、盘点（二号服务器只读，2026-11-18 00:40 左右实测）

二号记忆根目录：`/root/.pi/agent/bin` 的 l1_watcher.py（75,808 字节）写 L1；记忆总目录 `/root/.pi/agent/memory/` 共 37M。
l1_watcher.py 位置：`/root/.pi/agent/bin/l1_watcher.py`（1758 行，V6.1）。

### 1.1 L1 日志：7 个文件，390 行，51KB（只计 .md 内容）

| 文件 | 行数 | 字节 | ## 节数 | - Q: 数 |
|---|---|---|---|---|
| 2026-11-11.md | 102 | 14203 | 19 | 12 |
| 2026-11-12.md | 8 | 974 | 1 | 1 |
| 2026-11-13.md | 156 | 20110 | 29 | 19 |
| 2026-11-14.md | 52 | 7250 | 10 | 5 |
| 2026-11-15.md | 8 | 1097 | 1 | 1 |
| 2026-11-16.md | 34 | 4737 | 6 | 4 |
| 2026-11-17.md | 30 | 2792 | 6 | 2 |
| 合计 | 390 | 51163 | 72 | 44 |

格式要点：Markdown 文件。文件头 `# 2026-11-16`；每条为 `## [HH:MM] <标签>` 节，节内 `- Q: ...` / `A: ...` 两段；标签有 recent、[教训] 失败等。11-12 与 11-15 各只有 1 节（l1_watcher 回读缺陷导致当日漏采，见 pi-000934）。L1 目录另有 `.watcher_state.json`（1.1MB，watcher 运行时状态，非记忆内容，不迁）。

### 1.2 Wiki：348 个 .md 文件，1.9M

| 分类 | 文件数 | 大小 |
|---|---|---|
| concepts | 171 | 796K |
| entities | 50 | 284K |
| sources | 2 | 8K |
| synthesis | 113 | 548K |
| playbooks | 5 | 24K |
| 根文件（index.md、registry.md、log.md、lint-report.md） | 4 | 约 217K |
| _archive-index-pages | 3 | 12K |

格式要点：YAML frontmatter（`path:`、`category:`、`updated:`、`source:`）+ `# 标题` + `## Metadata` 块 + 正文。抽查 40 个文件全部带 frontmatter；0 个文件自带 `Summary:` 行（007 Recall 读 Summary 行，转换时须合成）。文件名无空格、无 `..`；最大文件 lint-report.md 125KB。注意：个别文件 frontmatter 的 `category:` 与所在目录不一致（如 concepts 目录下的文件 frontmatter 写 entities），映射以目录为准。

### 1.3 其他记忆目录（memory/ 下 37M 的构成）

| 目录 | 大小 | 文件数 | 处理 |
|---|---|---|---|
| system/（成就/状态 JSON） | 8.4M | 554 | 不迁：运行时状态，非记忆内容 |
| candidates/（未批准候选） | 8.3M | 2087 | 不迁：还不是记忆 |
| _archive/（l3-dup、l3-junk、wiki-dup 等） | 6.7M | 1538 | 不迁：重复与废弃件 |
| agent/（lessons、decisions、patterns、cases 等） | 4.8M | 1181（1180 .md + 1 .jsonl） | 迁：执行经验，见 2.3 |
| archive/（protocols 108 + 旧快照） | 3.1M | 约 120 | 迁 protocols 108 个，其余不迁 |
| wiki_candidates/（pending） | 1.1M | 149 | 不迁：未批准 |
| l3-*.tsv、backfill-*（L3 索引工作文件） | 约 1.4M | — | 不迁：007 自建三级索引 |
| governance/、goals/、user/、reflections/、CONTINUITY_SNAPSHOT.md | 约 60K | 约 12 | 迁：小件参考 |
| stages/、bundles/、sensory_cortex/、subconscious/、swarm_matrix/、transcendence/、autonomic_nervous/、assets/、adjudication/ | 小或空 | — | 不迁：空目录或工作状态 |

### 1.4 不迁的部分（明确记录）

- `~/.pi/agent/sessions/`（177M 原始会话 JSONL）：L1 已是其提炼，007 无会话库；177M 进仓库不合理。
- `MEMORY.md`：在二号服务器上没找到（`/root/.pi` 与 `/root` 4 层内无此文件），无物可迁。
- `node_modules/`（475M）、`install/`、`npm/`：运行环境，与记忆无关。

### 1.5 007 记忆的目标格式（本地 internal/memory/memory.go 实测）

- Store 根目录结构：`l1/`、`wiki/{entities,concepts,sources,synthesis}/`、`carryover/`；`Open()` 自动建目录（0700）。
- L1：`l1/YYYY-MM-DD.log`，每行 `RFC3339时间戳\t文本`（append-only，0600）；`ReadL1(日期)` 按日期读；`PruneL1()` 删 7 天前的文件（按文件名日期，`date < cutoff` 才删）。
- Wiki：`WriteWiki` 是唯一写入口；分类只认 entities/concepts/sources/synthesis 四种；词条名不含 `/`、`\`、`..`；内容格式 `# 标题\n\nSummary: 一句话摘要\n\n正文\n`；写完落一条 L1 日志；文件 0600。
- Recall：词法必查——Wiki 读 `Summary:` 行（无 Summary 行则回空摘要，但仍按全文计分）、L1 按行计分；top_k=3，每条 ≤240 字符；语义可注入，未注入只用词法。注意：Recall 用 `filepath.WalkDir` 扫整个 `wiki/`，所以 `wiki/` 下任意子目录的 .md 都能被词法召回（playbooks 与根文件同样可召回）。
- Rollover：会话 token >200000 时截断到 6000 字符写 carryover/，只留最近 8 条。
- 换页与 L1 的时效性注意：种子 L1 是 11-11～11-17 的日志，PruneL1 按 7 天清理——种子 L1 落盘后几天内会自然过期；耐久知识在 wiki，L1 只是 7 天工作日志。这是设计使然，不是迁移丢失。

## 二、映射方案（二号记忆 → 007 记忆）

### 2.1 私密库存放布局（只进 super-agent-007，绝不进公开库）

```
memories/no2-seed/                  # 二号种子记忆（一次性的起点语料）
  README.md                         # 来源、范围、映射说明
  MANIFEST.md                       # 数量、md5、日期
  l1/                               # 转换后的 L1：2026-11-11.log … 2026-11-17.log
  wiki/
    entities/ concepts/ sources/ synthesis/   # 转换后的 4 类 wiki（007 原生格式）
    playbooks/                      # 5 个剧本（同格式；Recall 可召回，WriteWiki 不写此类）
    index.md registry.md log.md lint-report.md   # 根文件，原样
    _archive-index-pages/           # 3 个，原样
  reference/                        # 参考资料（原样，不进 Store 的 4 类写入口）
    agent/                          # 1180 个 .md：lessons/decisions/patterns/cases 等
    protocols/                      # 108 个 .md：钢铁意志协议
    governance/ goals/ user/ reflections/
    CONTINUITY_SNAPSHOT.md
scripts/seed-memory.sh              # 首次部署接线脚本（见 2.5）
```

### 2.2 L1 转换规则

每条 `## [HH:MM] <标签>` 节转一行日志：`YYYY-MM-DDT HH:MM:00+08:00\t[HH:MM] <标签> <节正文压平>`。节正文压平：去行首 `- Q:` / `A:` 标记外的换行全部压成空格、多余空白折叠、原文保留（含已有的 `...` 截断，不补全）。文件头 `# 日期` 行丢弃。预期 72 行（19+1+29+10+1+6+6）。空节跳过并计数。

### 2.3 Wiki 转换规则（4 类 + playbooks）

- 词条名 = 文件名去 `.md` 后缀（与二号文件名一一对应，便于审计；文件名无空格/`..`，符合 WriteWiki 校验）。
- 内容改写为 007 原生格式：`# <词条名>\n\nSummary: <合成摘要>\n\n<正文>\n`。正文 = 去掉 YAML frontmatter、保留原 `# 标题` 行与 `## Metadata` 块；文末追加一行来源脚注（原分类、原 frontmatter path、原 updated）。
- Summary 合成规则：取正文第一条非空、非标题、非引用、非 `## Metadata` 块的行，去 markdown 标记（`**`、链接），截 120 字符；取不到则用 `二号 wiki 词条（原文见正文）`。
- 根文件与 _archive-index-pages 原样复制，不改写（它们是索引与日志，不是词条）。
- 转不过来的部分：无。全部 348 个文件均为 .md 且带 frontmatter，无异常文件名；frontmatter `category:` 与目录不一致的以目录为准（已在 1.2 注明）。

### 2.4 reference/ 存放规则

agent/（1180 .md）与 protocols（108 .md）及 governance/goals/user/reflections/CONTINUITY_SNAPSHOT.md 原样复制，保留子目录结构；agent/ 下唯一的 `RECALL_SIGNAL_LEDGER.jsonl` 不迁（账本类运行文件）。这些文件不进 Store 的 4 类写入口，但部署后 007 可用文件工具按需读取；其中 agent/ 的 lessons/decisions/patterns/cases 正是用户要的“执行经验”。

### 2.5 部署接线（种子怎么进守护进程读的 data/memory/）

- 仓库里只放种子，不直接放 `data/`（`data/` 是运行时目录，.gitignore 已屏蔽 `.db`/`.env`；公开库也绝不能出现）。
- 新增 `scripts/seed-memory.sh`（随私密库走）：首次部署时把 `memories/no2-seed/l1/` 与 `memories/no2-seed/wiki/` 用 `cp -a`（保留 mtime）拷进 `$DATA_DIR/memory/`（默认 `./data/memory`）；仅当目标不存在或为空时才拷，绝不覆盖已有的 `data/memory/`。
- 关键一步：拷完立即写备份标记文件 `$DATA_DIR/backup_memory_last`（内容为当前 RFC3339 时间）。原因：第 15 步备份的候选规则是“无标记时全量”——不写标记，第一次 04:00 备份会把 336 个种子 wiki 文件重复推一份进 `memories/<server_id>/`；写了标记且种子 mtime 是旧的，备份只推之后真正改动的文件。
- 种子与备份的关系：`memories/no2-seed/` 是起点语料（一次性，全库共享）；`memories/<server_id>/` 是各机增量（每天 04:00 精炼后推本机改动）。两者不混、不重复。

### 2.6 数量核对基线（全量时用）

L1：7 文件 → 7 个 .log，72 行；wiki：4 类 336 + playbooks 5 + 根文件 4 + _archive-index-pages 3 = 348 个；reference：agent 1180 + protocols 108 + 小件约 12 = 约 1300 个；种子总计约 1655 个文件，约 7MB。

## 三、试点验证（2026-11-18 00:35–00:50，本地 Go 1.26.0）

试点样本：10 个 wiki（concepts 3、entities 3、synthesis 2、sources 1、playbooks 1）+ 全部 7 个 L1 日志（72 行，含前 20 行时间戳解析）+ 1 个伪造旧日志（2026-11-01，用于 PruneL1）。

结果（临时测试 `internal/memory/seed_pilot_test.go` 与 `internal/llm/seed_pilot_test.go`，验完已删）：
- ReadL1：2026-11-16 读回 6 行；前 20 行时间戳全部通过 RFC3339 解析；7 文件共 72 行，与盘点一致。
- Recall（词法，top 3）：`RTK` → rtk-shell 概念页（score 30）；`持久卷` → l1/2026-11-12.log；`World-Map-Data` → l1/2026-11-15.log；`记忆写入链路` → wiki/playbooks/记忆知识/排障页（score 34）。4/4 命中，playbooks 同样可召回。
- WriteWiki 回写：2 个转换后文件解析回 WikiEntry 再写入，文件落盘、Summary 完整。
- RefineL1（httptest 桩模型）：回 2 条词条，WriteWiki 落盘成功。
- Rollover：200001 token 触发，摘要 ≤6000 字符，carryover 链 1 条。
- PruneL1：只删伪造的 2026-11-01，种子 11-11～11-17 全在。
- 全包测试：`go test ./internal/memory/ ./internal/llm/` 全过。

试点中抓出并修掉的问题：
1. 转换器初版 playbooks 只转了顶层 2 个，漏了子目录里的 3 个（记忆知识/系统运维/抓取调研各 1 个）——已改递归，复核 5/5。
2. 试点测试本身两次写错：实体文件名记错（DeepSeek插件非问答）、PruneL1 假文件日期按北京时间 11-18 算、本机却是 UTC 11-17——测试问题，非代码问题。顺带确认：PruneL1 按机器本地时间切（即复核 B-10 的时区项，P2），UTC 机上种子 L1 比北京时间早一天过期。

种子脚本验证（/tmp 模拟部署目录）：首次运行拷入 l1（7 个）+ wiki（全目录），写备份标记；第二次运行跳过；mtime 保留，标记时间晚于种子 mtime，备份不重推种子。

种子密钥扫描：`ghp_`/`sk-`/`AKIA`/`xox` 等模式零命中；含 `password` 的 6 个文件均为二号自带的打码指针（如 `[已归集凭据#K001 account_password qq97****893.]`）或字段名，无可用凭据值——二号记忆闸门本身就打码，可安全进私密库。

## 四、全量迁移 + 入库（2026-11-18 00:50–01:05）

### 4.1 全量核对数（转换器输出，与 2.6 基线逐项对上）

- L1：7 个 .log，72 行（19+1+29+10+1+6+6），跳过空节 0。
- Wiki：entities 50、concepts 171、sources 2、synthesis 113、playbooks 5（递归后补回子目录 3 个）+ 根文件 7 = 348 个。
- Reference：1305 个（agent 1180 + protocols 108 + governance 6 + goals 5 + user 4 + reflections 1 + CONTINUITY_SNAPSHOT.md 1）。
- 种子总计：1660 个文件（+ README.md 与 MANIFEST.md 两个说明文件），2,451,049 字节。Summary 合成兜底 4 个。
- 转换器留存：`~/workspace/super-agent-007/step16-convert.py`（可复跑）。

### 4.2 私密库同步（历史不同源，覆盖提交，未改历史）

- 核查：私密库 HEAD addaaba2 与公开库 main（8b54c6a）双向均非祖先——两库历史不同源（私密库只有 3 提交、4 文件），无法 fast-forward/merge。
- 做法：按第 13–15 步 126 文件清单覆盖到私密库工作区（md5 全对，0 失败），再加 `memories/no2-seed/`（1662 文件）与 `scripts/seed-memory.sh`，README 追加“私有构建说明”一节；单提交落在 addaaba2 之上，无 force-push、无历史改写。
- 全树密钥扫描：零命中；无 .env、ledger.db、*.db 文件。

### 4.3 推送与 API 核对（本地 GitHub token + 临时 GIT_ASKPASS 内存注入）

- 私密库 super-agent-007：`addaaba..0af477a`，新 HEAD `0af477a16a8eb5b5f52904a3014db72c4cddd789`，提交信息 `feat: sync framework to steps 13-15 + seed with #2 memory (L1 + wiki + experience)`，1790 个跟踪文件。
- API 核对：种子顶层 `MANIFEST.md、README.md、l1、reference、wiki` 齐全；种子 blob 1662 个；wiki 种子文件 348 个。
- 公开库 007-super-agent 未动：main 仍为 `8b54c6a`。pi-xinxin 未动。临时克隆与 askpass 脚本已清理。

### 4.4 未做 / 待办

- DeepSeek 密钥预配置不在本步（按任务要求只迁记忆）。
- 种子在真实部署上的首次接线（`scripts/seed-memory.sh`）只在 /tmp 模拟目录验过，未在真实服务器跑过——切流部署时跑一次即验证。
- 种子 L1 的 PruneL1 时区行为：按机器本地时间切（复核 B-10，P2），UTC 机上比北京时间早一天过期；不影响 wiki。
- 建议的后续（不在本步）：对种子 L1 跑一次真实模型精炼，把 72 行日志的耐久结论再提炼成 wiki（L1 本身 7 天过期）。

## 结论

第 16 步完成：二号记忆已全量迁入私密库 super-agent-007（新 HEAD 0af477a），框架代码与公开库同为第 13–15 步，种子 1660 个文件、72 行 L1、348 页 wiki、1180 页执行经验全部核对无丢失；试点 7 项验证全过；公开库未动。私密库现在是带着二号全部记忆的新二号种子，拉取后跑 `scripts/seed-memory.sh` 即可接进守护进程。
