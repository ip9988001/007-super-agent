# wiki_upsert_page（写 wiki）

> **一句话**：把稳定知识写成/更新成 wiki 页（4 类：entities / concepts / sources / synthesis），是记忆的"结构化真值"层。

## 实际效果（实测）
- 工具存在并在 `steel-will-30-wiki-engine.ts` 注册；参数：`title, category, content, summary?, tags?, source_path?`。
- 落点：`/root/.pi/agent/memory/wiki/<category>/<slug>.md`，同时更新 `wiki/index.md` 与 `wiki/log.md`。
- 现状（实测）：wiki 只有 **2 页** —— `concepts/auto-synth-lesson.md`、`sources/auto-synth-fact.md`，
  都是自动合成产物；`entities/`、`synthesis/` **为空**，`index.md` 标 `_Empty_`。
- 配套 `wiki_lint` 实测返回 **6 条 findings**（报告 `wiki/lint-report.md`）：
  `vector-index-empty`(MEDIUM) · `vector-wiki-sample-mismatch` ×2 · `pending-manual-review` · `missing-tags` ×2。

## 怎么调
```
wiki_upsert_page({title, category:"concepts"|"entities"|"sources"|"synthesis", content, summary, tags})
wiki_capture_candidate({text, source_session})   # 碎片先入 pending，别直接写 wiki
wiki_lint({})                                    # 写完体检（会落 lint-report.md）
recall_memory({query})                           # 验证能不能召回
```
分类口径：**entity**=人/工具/服务；**concept**=方法/阶梯/原则；**source**=外部资料出处；**synthesis**=跨源综合结论。

## 坑
- **别把碎片塞进 wiki**：临时信号走 `wiki_capture_candidate`（→ `wiki_candidates/pending/`，现有 2 条待审），
  wiki 只放稳定可复用的结论。
- **标题即主键**：同名 upsert 会覆盖整页，没有备份与 diff（`wiki/log.md` 只 99 字节）。
- `category` 写错会新建一个陌生目录，`index.md` 也跟着长出无名分区。
- **没有向量索引**：写进去的页不会进 `vector_db`（`items` 仍为空），所以 `recall_memory` 的 semantic 模式搜不到它，
  只能靠文件名/正文词法命中 —— 写完务必用 `recall_memory` 实测一次。
- `wiki_lint` 的 `pending-manual-review` 指 `wiki_candidates/pending/`，**不会自动清理**，会一直报。
- 本次未做真实写入测试（避免污染 wiki），以上为 schema + 现有 2 页 + lint 报告的实测归纳。
