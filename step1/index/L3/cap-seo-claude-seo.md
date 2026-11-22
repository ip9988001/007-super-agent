# cap-seo-claude-seo

## 一句话
26 个 SEO skill + 19 个 agent + 60 个 py 脚本的技能库（v2.5.0，AgriciDaniel/claude-seo）；所有脚本走 `/data/state/claude-seo/.venv` 托管环境，不污染系统 python。

## 怎么调
export CLAUDE_SEO_DATA_DIR=/data/state/claude-seo # 硬前提，不设永远 setup required cd /data/workspace/seo-toolkit/claude-seo python3 scripts/runtime.py doctor # 51ms python3 scripts/runtime.py run fetch_page.py <url> --json python3 scripts/runtime.py run portability_check.py # 校验 26 个 SKILL.md 跨 harness 合规 # 想恢复常规入口（本次未做，因要求不改配置）：git show HEAD:scripts/claude-seo > scripts/claude-seo && chmod +x scripts/…

## 坑
根因：`/data/start.sh:93` 用 `printf ... > /usr/local/bin/claude-seo` 写包装脚本，而该路径已是指向 `scripts/claude-seo` 的软链 → `…；仓库带 `.git` → `_is_plugin()` 判 False → 模式回落 `manual`，data_dir 取 repo 根 → `repo/.venv` 不存在 → 无 env 时永远 `setup r…

## 实际效果
入口链断了：`/usr/local/bin/claude-seo` → 软链 → `scripts/claude-seo`，而后者已被覆盖成 2 行 `exec 自己` 的转发脚本 → 无限 exec 循环，零输出不退…；真启动器在 git HEAD（75 行，自动挑 py 3.11–3.14）。取出来跑：`Runtime: ready / Python 3.11 / Chromium: ready` ✅ 说明坏只坏在被覆盖的那个文件

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /seo；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：入口链断了：`/usr/local/bin/claude-seo` → 软链 → `scripts/claude-seo`，而后者已被覆盖成 2 行 `exec 自己` 的转发脚…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/03-SEO与内容/S-claude-seo.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：26 skill + 19 agent + 60 py 脚本（v2.5.0）
