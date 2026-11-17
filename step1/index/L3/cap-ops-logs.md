# cap-ops-logs

## 一句话
查故障先看 `/data/logs/`，但只有 4/6 个候选日志真的存在，网关日志根本不落盘。

## 怎么调
tail -40 /data/logs/boot.log # 引导结果（每段都有"就绪"行） tail -f /data/logs/l1-watcher.log # L1 采集是否活着（心跳 5s 一次） grep -c "就绪" /data/logs/boot.log # 数成功段 docker logs <ctr> --tail 100 # 网关日志只能从容器外拿

## 坑
网关没有任何落盘日志：`bot-gateway.js` 用 `console.log('['+ts()+']', ...)` 全走 stdout；日志不会轮转：`boot.log` 已 7805 行、`semgrep-install.log` 380K，`audit73.jsonl` 这类批量产物 11–18MB 各一个

## 实际效果
核实：Step 1 文档已读（未独立实测运行时）（2026-11-17）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：退出码 0；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/L-logs.md · 核实：2026-11-17（Step 1 文档已读（未独立实测运行时））
> 注意：只有 4/6 个候选日志真的存在，网关日志根本不落盘
