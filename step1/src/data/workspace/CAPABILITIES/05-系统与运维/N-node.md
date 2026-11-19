# node v22.23.4

> **一句话**：网关、37 个 pi 扩展、全部前端工具链的唯一运行时。

## 实际效果（实测）
- `node -v` = `v22.23.4`；`/data/bot-gateway.js`（PID 7）与 `link-agent.cjs`（PID 565）都跑在它上面。
- 全局 npm 包（`/usr/local/lib`）：`@ast-grep/cli@0.51.0`、`corepack@0.36.0`、`npm@10.9.9`。
- pi 本体在 `/data/node_modules/.bin/pi`（持久卷），`/usr/local/bin/pi` 是软链。

## 怎么调
```
node -v
npm ls -g --depth=0        # 看全局装了什么
npm i -g @ast-grep/cli     # 全局 CLI（不持久，见坑）
node /data/bot-gateway.js  # 容器主进程（start.sh 5 段 exec 它）
```

## 坑
- **全局 npm 包不在持久卷**：容器重建后 `/usr/local/lib/node_modules` 清空 →
  `ast-grep` 靠 `start.sh` 2i 段 `npm i -g @ast-grep/cli` 重新兜，其余全局包没人兜。
- `start.sh` 里 `ln -sf /data/node_modules/.bin/pi /usr/local/bin/pi` 是**唯一**的 pi 入口重建动作，
  删了 `/usr/local/bin/pi` 就等于没 pi。
- 网关是 `exec node ...` 的前台进程，日志走 stdout（见 `L-logs.md`），没有落盘文件。
