# content 三件套
## 一句话
claude-seo 里三个纯文本内容工具（吃 txt/md/html，支持 stdin 与 `--json`）：`content_quality.py` 出 0–100 质量分 · `content_humanize.py` 去 AI 腔 · `content_verify.py` 查主张与引用缺口。

## 实际效果（实测 2026-10-26，全部经 `runtime.py run` 调用）
- `content_quality.py` 判别力真实（拿 geo skill 的两个 fixture 跑，同输入同结果）：
  - strong-page 正文 → **Overall 85/100**（信息密度 0.90、AI-pattern 0、重复 28、122 tokens）
  - weak-page 正文 → **Overall 66/100**（信息密度 0.21、AI-pattern 15 命中 "cutting-edge"、重复 5、47 tokens）
  - 两者都挂 `thin-content` → 这是字数门限，不是质量结论
- `content_humanize.py` 实跑 weak 文本 → `--- 1 replacements ---  'cutting-edge' -> 'modern'`，默认把洗后全文打到 stdout ✅
- `content_verify.py` 实跑 strong 文本 → `Claims: 3 / Uncited: 0 / Uncited ratio: 0.00`，逐条 `[✓ quantity] '10,000 requests'` ✅
- `nlp_analyze.py` 走 Google Cloud Natural Language API（`--api-key`），**无 key 不可用**
- 直接喂 HTML 也能跑，但分数会被标签文本污染：同一 strong-page 喂 .html 得 80/100、269 tokens（先剥标签再喂得 85/100、122 tokens）

## 怎么调
```bash
export CLAUDE_SEO_DATA_DIR=/data/state/claude-seo      # 不设会 Runtime not ready
cd /data/workspace/seo-toolkit/claude-seo
python3 scripts/runtime.py run content_quality.py draft.md                # 人读版
python3 scripts/runtime.py run content_quality.py draft.md --json --threshold 70
python3 scripts/runtime.py run content_humanize.py draft.md -o clean.md   # 去 AI 腔，不覆盖原文件
python3 scripts/runtime.py run content_verify.py draft.md --json
echo "要打分的文本" | python3 scripts/runtime.py run content_quality.py -
```
顺序建议：`content_quality` 先看分与 flags → `content_humanize` 洗稿 → `content_verify` 补引用 → 再跑一次 quality 看回升。

## 坑
- 指标是可解释启发式，不是 LLM 判断：`thin-content` 对短文案一律报，<200 词的落地页别当真分。
- `--threshold` 只影响退出码/提示，不改分数；接 CI 时看退出码别看文本。
- `content_humanize` 只写 `-o` 指定的文件，**不建 .bak**；要覆盖原稿自己先备份。
- 三个脚本都不做 gzip 解压（同 `G-geo-seo-aeo.md` 的坑）：只喂本地文本，别把裸 gzip 字节管道进来。
- 与 `audit.py` 的重叠只有"模糊词/量化信号"两项；内容质量总分以 `content_quality.py` 为准，别两处各报一个分。
