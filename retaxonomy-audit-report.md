# 007 注册表重新分类核查报告（v1.6 → v1.7）

生成时间：2026-11-18
范围：84 条注册表逐条核查 + 测试机 `tools/skills/` 40 个 SKILL.md 实查。
原则：不删任何条目，不减任何工具；只改分类与描述；描述只写注册表里有证据的功能，不确定的标 [待核实]。

## 执行结果

- 新注册表：`step1/registry/registry-v1.7.json`（v1.7，84 条，顺序与 tool_id 与 v1.6 完全一致；除 category、subcategory、one_liner、version、generated 外，其余字段与 v1.6 逐字节一致，已程序校验）
- 中文备份：`registry-v1.7-zh.md`（9 大类说明、17 子类说明、84 条工具定义、40 个 skill 分类）
- 新分类：9 大类（fetch 13、code 8、seo 7、design 8、security 5、runtime 7、media 6、ops 16、memory 14），17 子类；可调工具 47 个
- 标 [待核实] 的条目：10 条（见第 5 节）

## 0. ⚠️ 部署前置条件（最重要：v1.7 直接部署会启动失败）

**v1.7 不能直接替换 registry.json 部署。** Go 加载器 `internal/registry/registry.go` 硬编码了 6 类白名单：

- `categoryOrder`（第 44 行）= `["fetch", "code", "seo", "design", "ops", "memory"]`
- `validCategory`（第 46-49 行）= 同上 6 类
- `validateEntry`（第 174 行）：`if !validCategory[e.Category]` 直接返回 `unknown category` 错误，`Load` 失败，守护进程无法启动。

v1.7 新增 security、runtime、media 三类，现有代码加载即报错。部署前主 agent 必须改 Go 代码（不在本次任务范围内）：

1. `categoryOrder` 加 security、runtime、media（顺序建议：fetch、code、seo、design、security、runtime、media、ops、memory）。
2. `validCategory` 同步加三类。
3. `registry_test.go` 第 79-88 行：断言 6 类与每类条数（fetch 13、code 8、seo 7、design 8、ops 34、memory 14）须改为 9 类与新条数（ops 34→16，新增 security 5、runtime 7、media 6）。
4. 注意第 43 行注释：6 类与 `index/L2/` 下 6 份文件一一对应。L2 现有 7 份文件（code、design、fetch、memory、ops、ops-2、seo），新增 3 类是否需要补 L2 文件，须主 agent 核查 `index/L2/` 的引用逻辑后再定。
5. 另：`registryFile` 结构用 `json.Unmarshal`（非严格模式），本次新增的顶层 `categories`/`subcategories` 字段会被静默忽略，不影响加载；`Load` 校验 total=84 与条数一致、tool_id 唯一、必填字段齐全，v1.7 均已通过（本地程序校验）。

未改 Go 代码前，**不要**把 registry-v1.7.json 覆盖到测试机 `data/registry/registry.json`。

## 1. 方案本身的算术错误（实施时发现并纠正）

1. **ops 可调数写错。** 方案总览表称 ops（16 条）可调 9 个，实际只有 7 个（16 条里 doc 3、sop 3、cli 不可用 2、service 不可用 1，剩下 7 个 service 可用）。方案按 9 个算时各大类可调数相加为 49，与方案自己写的总数 47 矛盾。v1.7 按实际 7 个执行，总数 47 无误。
2. **子类总数写错。** 方案称子类“从 21 个变 22 个”，实际新分类只有 17 个子类（fetch 3、code 3、seo 2、design 3、security 1、runtime 1、media 1、ops 1、memory 2 = 17）。旧 21 个里 ops 的 7 个子类合并为 4 个大类下的 4 个子类，code 的 code-search 与安全审计合并为 2 个，净减 4 个，21 - 4 = 17。v1.7 按 17 个执行。

## 2. 描述与备注自相矛盾

3. **cap-fetch-gh-cli**：one_liner 称“未登录且无 GH_TOKEN，所有 API 命令当前不可用”，note 却称“2026-11-17 复核已登录，以复核为准”。两者直接矛盾。v1.7 的英文描述保留矛盾并标 [TO VERIFY]，须实测 `gh auth status` 后定稿。
4. **cap-fetch-bili**：verified_basis 写“实测 2026-10-26”，note 却写“最小调用未出数据，未验证”。验证依据与备注冲突，available=False 是对的，但验证依据应视为过期。

## 3. entry_point 是展示字符串，不是可执行入口

以下条目的 entry_point 是描述文字，无法直接执行（call_type 为 data/sop/mcp/doc 的，本来就不进模型可调清单，影响有限；但 cli 型的须注意）：

5. **cap-seo-design-md**（data）：“84 份 DESIGN.md 库”——是描述，不是路径。
6. **cap-seo-llms-txt**（sop）：“llms.txt 模板+规程”——是描述，不是路径。
7. **cap-seo-seo-gateway**（sop）：“/seo 路由”——是路由名，不是命令。
8. **cap-fetch-scrapling-mcp**（mcp）：“stdio scrapling-mcp server”——是描述；真实路径在 note 里（/data/tools/uv/bin/scrapling-mcp）。
9. **cap-memory-playbooks**：entry_point 就是一个破折号“—”，available=False，是幽灵条目（见第 6 节）。
10. **cap-fetch-httpx**（cli，可调）：entry_point 是裸 `httpx`，但 httpx 是 Python 库，不是独立命令，PATH 上很可能没有可执行的 httpx。实际用法是 Python 里 import，须走 python3 调用。入口写法可疑，v1.7 描述已写明“须在 Python 里调用”，建议后续核实 PATH 上是否有 httpx 可执行文件。

## 4. 重复条目（同一实现，两个注册表条目）

11. **cap-memory-recall-memory [57] vs ext-recall-memory [59]**：同一 recall_memory 实现。
12. **cap-memory-wiki-upsert-page [58] vs ext-wiki-upsert-page [60]**：同一 wiki_upsert_page 实现。
13. **cap-code-codegraph [13] vs ext-codegraph [65]**：同一二进制（note 自己写明“与另一条 codegraph 同一真身”）。

三组重复均保留未删（按任务约束），v1.7 描述里已写明互为重复、二者选其一。建议后续由主 agent 决定是否合并。

## 5. 标 [待核实] 的 10 条

| tool_id | 原因 |
|---|---|
| cap-fetch-bili | 最小调用未出数据，未验证 |
| cap-fetch-gh-cli | 登录状态描述与备注矛盾 |
| cap-fetch-tvly | 无 Tavily key，三号上同样不可用 |
| cap-seo-claude-seo | 007 上无 .venv 与数据目录，setup/doctor 报缺 |
| cap-seo-toolkit-index | 路由表有几处已过期 |
| cap-design-hyperframes | 未在三号上实测跑通过，无能力文件；且缺 last_verified_at（84 条里唯一缺失） |
| cap-memory-playbooks | playbooks 目录在盘上不存在，零实体 |
| ext-wiki-lint | 在 settings.json defaultTools 的 - 名单上，该前缀语义未验证 |
| ext-usage-report | 同上，- 名单语义未验证 |
| ext-detect-conflicts | 同上，- 名单语义未验证 |

## 6. 分类存疑（按方案执行，但值得复议）

14. **cap-ops-ast-grep** 归入 security/scan：ast-grep 是通用 AST 搜索与改写工具，不是专职安全扫描器；按功能更贴近 code/search。方案映射要求放 security，v1.7 照办。
15. **cap-design-ui-visual** 归入 design/ui-gen：它是截图验证规程（chrome-nosandbox --screenshot / Playwright），按功能更贴近 visual-check。方案未单列，v1.7 照旧。
16. **cap-design-gateway-timeouts** 归入 design/visual-check：它是一份网关超时机制说明文档，与设计无实质关系。v1.7 照旧。
17. **skill-migration、skill-forge** 归入 ops/ops-mgmt：方案正文称 5 个核心 skill 都在 memory 大类，但按方案的数量表（memory 14、ops 16）这 2 个只能留在 ops。方案正文与数量表不一致，v1.7 按数量表执行。
18. **cap-code-semgrep** 归入 code/audit：方案 security 大类的说明文字里提到 Semgrep，但 semgrep 条目实际在 code/audit。文字与分类不完全一致，属方案行文问题。

## 7. 缺失与过期信息

19. **note 为空的 4 条**：cap-ops-trivy、ext-recall-memory、ext-wiki-upsert-page、ext-self-status。
20. **cap-design-hyperframes 缺 last_verified_at**：84 条里唯一缺失（见第 5 节）。
21. **cap-seo-toolkit-index 路由表过期**：note 自承“有几处已过期”。
22. **cap-design-impeccable-skill**：24 个命令只有名字和规程，CLI 里除 detect 外一个都没实现。
23. **cap-code-codegraph**：007 上暂无现成索引，用前须先对目标仓库建索引；note 里的路径是三号的 /data/workspace/rea。
24. **cap-seo-claude-seo**：007 上无 .venv 与数据目录（见第 5 节）。
25. **param_schema 为空的 73 条**：84 条里 73 条 param_schema 是 `{}`，只有 11 条有参数结构。模型因此拿不到大多数工具的参数约定（本次未改，超出重分类范围，建议后续补）。

## 8. 不可用条目（6 条，均保留未删）

26. cap-fetch-bili（未验证）、cap-fetch-tvly（无 Tavily key）、cap-ops-lean-ctx（已死）、cap-ops-llmtrim（已死）、cap-memory-playbooks（幽灵条目，目录不存在）、cap-ops-script-link-selftest（三号专属）。

## 9. 脚本硬编码三号路径（6 条，均已在描述中注明）

27. cap-ops-script-cap-drift（/data/workspace/CAPABILITIES，第 2 节在 007 上为空）、cap-ops-script-cap-scan（/data/tools/*/bin）、cap-ops-script-chrome-cdp（/data/cache/ms-playwright/chromium-*）、cap-ops-script-chrome-nosandbox（三号版路径，007 实跑走 /usr/local/bin/chrome-nosandbox）、cap-ops-script-inventory（/data/bot-gateway.js、/data/tools/*）、cap-ops-script-inventory2（/data/bot-gateway.js、/root/.agents/skills/*）。v1.7 的每条描述里都写明了路径 caveat。

## 10. 子类改名清单（本次执行）

- http-search → http；code-search → search；安全审计 → audit；security-scan → scan
- runtimes、语言运行时 → lang；doc-media、文档与媒体 → doc-media；ops-mgmt、运维管理 → ops-mgmt（两组不同工具合并为一个子类）
- status_triad 分布：62 条 installed+runnable+verified 全真；13 条 verified=false；7 条 runnable=false；2 条 installed=false。available=False 的 6 条见第 8 节。

## 建议后续（不属于本次任务）

1. 实测 `gh auth status`，解决 cap-fetch-gh-cli 的登录矛盾；实测 bili 最小调用、hyperframes 渲染、Tavily key 有无。
2. 三组重复条目（第 4 节）由主 agent 决定是否合并。
3. 补 73 条空 param_schema，模型才能拿到参数约定。
4. 6 条硬编码三号路径的脚本，改路径或在注册表里标注 007 版入口。
5. 第 6 节 5 个分类存疑点，由用户或主 agent 复议。
