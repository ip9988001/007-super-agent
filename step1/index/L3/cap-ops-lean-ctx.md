# cap-ops-lean-ctx

## 一句话
终端命令与文件阅读的降噪/压缩网关（单二进制），rtk 已退役，由 lean-ctx 接管。

## 怎么调
lean-ctx -c "cmd" # 跑命令 + 压缩输出（日常主力） lean-ctx raw "cmd" # 同上但原始输出（逃生口，等价 LEAN_CTX_RAW=1） lean-ctx read <file> # 压缩式读文件 lean-ctx grep <pat> # 压缩式搜索 lean-ctx overview # 项目结构速览 lean-ctx gain --deep # 省钱/省 token 明细 lean-ctx tools health # 未用工具 / 冗余规则 / 过期知识体检 lean-ctx status|doctor # 是否接线正常

## 坑
状态目录靠软链持久化：`/root/.local/share/lean-ctx` → `/data/state/lean-ctx/share`；`lean-ctx serve` 守护进程瞬时 CPU 很高（实测 `ps` 抓到 536%），独核运行时可感知变慢。

## 实际效果
`lean-ctx -c "ls -la <dir>"` → 输出尾部标 `277 → 109 tok (↓~60%)`，9 个文件一条不丢。；`lean-ctx overview` → 直接给项目树（depth 2，含每目录文件数），省掉一串 `ls`。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`lean-ctx -c "ls -la <dir>"` → 输出尾部标 `277 → 109 tok (↓~60%)`，9 个文件一条不丢。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/L-lean-ctx.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：实测累计省 1.2K token、22 天零使用；能力表 60% 为单样本，不作主力省 token 手段
