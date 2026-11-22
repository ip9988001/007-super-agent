# cap-memory-wiki-upsert-page

## 一句话
把稳定知识写成/更新成 wiki 页（4 类：entities / concepts / sources / synthesis），是记忆的"结构化真值"层。

## 怎么调
wiki_upsert_page({title, category:"concepts"|"entities"|"sources"|"synthesis", content, summary, tags}) wiki_capture_candidate({text, source_session}) # 碎片先入 pending，别直接写 wiki wiki_lint({}) # 写完体检（会落 lint-report.md） recall_memory({query}) # 验证能不能召回

## 坑
别把碎片塞进 wiki：临时信号走 `wiki_capture_candidate`（→ `wiki_candidates/pending/`，现有 2 条待审）；标题即主键：同名 upsert 会覆盖整页，没有备份与 diff（`wiki/log.md` 只 99 字节）。

## 实际效果
工具存在并在 `steel-will-30-wiki-engine.ts` 注册；参数：`title, category, content, summary?, tags?, source_path?`。；落点：`/root/.pi/agent/memory/wiki/<category>/<slug>.md`，同时更新 `wiki/index.md` 与 `wiki/log.md`。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：工具存在并在 `steel-will-30-wiki-engine.ts` 注册；参数：`title, category, content, summary?, tags?, s…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/06-记忆与知识/W-wiki-upsert-page.md · 核实：2026-11-17（2026-11-17 审计：扩展已加载（日志命中））
> 注意：与扩展工具同一实现，勿重复注册
