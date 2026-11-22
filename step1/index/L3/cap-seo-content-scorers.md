# cap-seo-content-scorers

## 一句话
claude-seo 里三个纯文本内容工具（吃 txt/md/html，支持 stdin 与 `--json`）：`content_quality.py` 出 0–100 质量分 · `content_humanize.py` 去 AI 腔 · `content_verify.py` 查主张与引用缺口。

## 怎么调
export CLAUDE_SEO_DATA_DIR=tools/seo-toolkit/claude-seo-data # 不设会 Runtime not ready cd tools/seo-toolkit/claude-seo python3 scripts/runtime.py run content_quality.py draft.md # 人读版 python3 scripts/runtime.py run content_quality.py draft.md --json --threshold 70 python3 scripts/runtime.py run content_humanize.py draft.md -o clean.md # 去 AI 腔，不覆盖原文件 python3 scripts/runtime.py run content_verify.py…

## 坑
指标是可解释启发式，不是 LLM 判断：`thin-content` 对短文案一律报，<200 词的落地页别当真分。；`--threshold` 只影响退出码/提示，不改分数；接 CI 时看退出码别看文本。

## 实际效果
`content_quality.py` 判别力真实（拿 geo skill 的两个 fixture 跑，同输入同结果）；strong-page 正文 → Overall 85/100（信息密度 0.90、AI-pattern 0、重复 28、122 tokens）

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /seo；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`content_quality.py` 判别力真实（拿 geo skill 的两个 fixture 跑，同输入同结果）；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/03-SEO与内容/C-content-scorers.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：三个纯文本内容工具，吃 txt/md/html，支持 stdin 与 --json
