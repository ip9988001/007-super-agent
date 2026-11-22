# cap-design-ui-route

## 一句话
`/ui [--deep] [--write] <URL|路径> [附加说明]` → detect 确定性打分 → 规则聚合 → pi 出修复包 三段串行，纯只读（除非显式 `--write`），源码 `bot-gateway.js:471-540`。

## 怎么调
# 等价的手工复跑（先只跑第 1 段，省钱省时） export IMPECCABLE_BROWSER=/usr/local/bin/chrome-nosandbox /root/.pi/agent/skills/impeccable/scripts/impeccable detect --json "https://example.com" | tail -c 2000 # 只读体检（本地项目） /ui /data/workspace/mysite # 深度：多读 critique.md + craft-floor.md，时间预算翻倍，检测规则不变 /ui --deep https://example.com # 写盘（危险，见下） /ui --write /data/workspace/mysite

## 坑
`--deep` 名不副实：detect 命令、参数、规则集完全一样，只是超时从 150→240s、pi 从 180→420s，外加让 pi 多读两篇 reference。规则不会变多。；静默假绿灯（最坑）：detect 超时/失败（没设浏览器、目标 404、站点过重）→ stdout 空 → `findings=[]` → 头行写 `命中 0 条`，prompt 里写"（无命中：目标不可访问…）"。"…

## 实际效果
第 1 段 detect：`run('…/scripts/impeccable',['detect','--json', target], {timeoutMs: deep?240000:150000, env:{IM…；参数只有 `--json`：不传 `--viewport`（永远桌面 1280x800，移动端问题扫不出来）、不传 `--scope`、不传 `--no-config`

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /ui；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：第 1 段 detect：`run('…/scripts/impeccable',['detect','--json', target], {timeoutMs: deep?24…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/04-设计与UI/G-ui-route.md · 核实：2026-11-17（Step 1 文档已读（未独立实测运行时））
> 注意：detect 打分 → 规则聚合 → pi 出修复包，三段串行；纯只读，除非显式 --write
