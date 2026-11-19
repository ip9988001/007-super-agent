# TOOLKIT-INDEX.md
## 一句话
`/data/workspace/seo-toolkit/TOOLKIT-INDEX.md`（85 行）是整条 seo 工具链的唯一路由表；`/seo` 路由每次都把它的前 140 行塞进 prompt。它是事实来源，但有几处已过期。

## 实际效果（逐条核对 2026-10-26）
| 索引声明 | 核对结果 | 结论 |
|---|---|---|
| 生成时间 `2026-10-25T22:47:45` | 全 workspace（js/sh/py）搜不到生成器 | 一次性手写/生成，**不会自更新** |
| claude-seo `v2.5.0` · 26 skill · 19 agent · 60 py | skills 26 / agents 19 / scripts 60 / plugin.json 2.5.0 | ✅ 全对 |
| geo-seo-aeo-skill `003b514` | `git rev-parse --short HEAD` = `003b514` | ✅ |
| env 三变量"已写入 start.sh" | start.sh:89-91 三行俱在 | ✅ |
| 入口 `scripts/claude-seo doctor` 与 `... run <script>` | 该启动器已被覆盖成自转发，两条都跑不通 | ❌ 入口段整体失效 |
| audit.py 维度：geo 含「问答/FAQ 结构提取」 | 问答/FAQ 在 `aeo` 段，不在 geo | ⚠️ 归错段 |
| （未写）audit.py 不处理 gzip | python.org 直连全零、词数假值 1928 | ❌ 缺最关键的一条坑 |

- 26 个 skill 的「skill → SKILL.md 路径」列是准的，按需读单文件即可 ✅
- 结论：**路由表可用、入口命令不可用**；把它当目录，别当执行手册。

## 怎么调
```bash
sed -n '1,140p' /data/workspace/seo-toolkit/TOOLKIT-INDEX.md   # 85 行，一次读完正好
grep -n "seo-geo\|seo-technical" /data/workspace/seo-toolkit/TOOLKIT-INDEX.md  # 找 skill 的 SKILL.md 路径
```
照抄入口命令前替换成可用形式（详见 `S-claude-seo.md`）：
```bash
export CLAUDE_SEO_DATA_DIR=/data/state/claude-seo
cd /data/workspace/seo-toolkit/claude-seo
python3 scripts/runtime.py doctor        # 替代 scripts/claude-seo doctor
python3 scripts/runtime.py run <x>.py    # 替代 scripts/claude-seo run <x>.py
```

## 坑
- **静态快照**：仓库 HEAD 已到 `219ff48 docs(security): installer verification moves to v2.6.0`，索引仍按 v2.5.0 写；plugin.json 本身也是 2.5.0，所以数字没错，但以为"索引=最新"会误判。
- 表里 skill 描述是从 SKILL.md frontmatter 机械截取的，部分行以 `>` 开头（如 `seo-agentic`、`seo-cluster`），照抄会把 `>` 带进输出。
- `/seo` 只喂前 140 行 —— 文件一旦超过 140 行，后面新增的内容对路由不可见。
- 更新它没有脚本：改了工具链（版本/入口/env）必须手改这一个文件，否则网关还会继续喂过期信息。
