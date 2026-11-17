# cap-ops-node

## 一句话
网关、37 个 pi 扩展、全部前端工具链的唯一运行时。

## 怎么调
node -v npm ls -g --depth=0 # 看全局装了什么 npm i -g @ast-grep/cli # 全局 CLI（不持久，见坑） node /data/bot-gateway.js # 容器主进程（start.sh 5 段 exec 它）

## 坑
全局 npm 包不在持久卷：容器重建后 `/usr/local/lib/node_modules` 清空 →；`start.sh` 里 `ln -sf /data/node_modules/.bin/pi /usr/local/bin/pi` 是唯一的 pi 入口重建动作

## 实际效果
`node -v` = `v22.23.4`；`/data/bot-gateway.js`（PID 7）与 `link-agent.cjs`（PID 565）都跑在它上面。；全局 npm 包（`/usr/local/lib`）：`@ast-grep/cli@0.51.0`、`corepack@0.36.0`、`npm@10.9.9`。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`node -v` = `v22.23.4`；`/data/bot-gateway.js`（PID 7）与 `link-agent.cjs`（PID 565）都跑在它上面。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/N-node.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：v22.23.4；bot-gateway 与 link-agent 都跑在它上面
