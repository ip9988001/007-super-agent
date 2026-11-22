# cap-memory-extensions-overview

## 一句话
`/root/.pi/agent/extensions/*.ts` 共 37 个 TypeScript 扩展，每个在 `session_start`（少数挂 `session_end`/`tool_call`）时注册工具或后台逻辑。

## 怎么调
ls /root/.pi/agent/extensions/*.ts | wc -l # 37 grep -l 'name: "' /root/.pi/agent/extensions/*.ts # 哪些扩展暴露工具 grep -o 'on(.[a-z_]*' <file>.ts | sort -u # 该扩展挂的事件钩子

## 坑
改扩展必须重启 pi 进程才生效（`session_start` 才加载），改完不重启 = 没改。；状态写在 `/root/.pi/agent/memory/system/*.json`（`achievements / temporal / token_roi / usage / auto_promoter_stat…

## 实际效果
`grep -c 'name: "…"'` 合计 48 处，去掉 schema 里的 `keywords/aliases/summary/reflections/lessons/cases` 噪声，实际可用工具约 40…；全部只做「注册工具 / 挂 `session_start` 钩子」，没有常驻进程；进程级常驻只有 `l1_watcher.py` / `refine_daily.py`（见 `L-l1-watcher.md`）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`grep -c 'name: "…"'` 合计 48 处，去掉 schema 里的 `keywords/aliases/summary/reflections/lessons/…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/06-记忆与知识/E-extensions-总览.md · 核实：2026-11-17（Step 1 文档已读（未独立实测运行时））
> 注意：只做注册/挂钩子，无常驻进程
