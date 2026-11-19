# yarn
## 一句话
yarn 1.22.22（**classic 1.x**，不是 berry 2+）已经装好，和 npm 10.9.9 共用同一个 node v22.23.4；本机/本项目**不用它**，一律 npm。

## 实际效果（实测 2026-10-26）
- `yarn --version` → `1.22.22` ✅
- `npm -v` → `10.9.9`；`node -v` → `v22.23.4`（同一运行时，yarn 只是另一个包管理器前端）
- `yarn global dir` → `/usr/local/share/.config/yarn/global`（全局包与 npm 的 `/usr/local/lib/node_modules` **不是一个地方**）
- 现有全局依赖（`L-lean-ctx` / `N-node` 已记）：`@ast-grep/cli@0.51.0`、`corepack@0.36.0`、`npm@10.9.9` —— 没有 yarn 的贡献
- `yarn --version` 会顺带打 `(node:9965) [UNDICI-EHPA] Warning: EnvHttpProxyAgent is experimental`：node 24 侧代理告警，无害

## 怎么调
结论先行：**别用**。项目里只有 `package-lock.json` 生态（`/data/node_modules`、`/data/bot-gateway.js`、`/data/node_modules/.bin/pi`）。
```bash
npm -v && node -v                 # 需要确认运行时版本，用这两条
npm ls -g --depth=0               # 看全局装了什么
```
真要临时用（不推荐）：`yarn --version` / `yarn -v` 只读无副作用；`yarn install` 会生成 `yarn.lock` 并与 `package-lock.json` 打架，**不要在本机跑**。

## 坑
- 装了 ≠ 该用：yarn 1.x 与 npm 是**两套 lockfile + 两套全局目录**，混用会出现"npm 装完 yarn 找不到"。
- yarn 1.22 已停止演进，新项目要么 npm 要么 pnpm/yarn berry；本机没有 berry。
- `yarn global add` 装进 `/usr/local/share/.config/yarn/global`，**不在持久卷**，容器重建即丢（同 `N-node.md` 的坑）。
- 用 corepack 切 yarn 版本会改写 `/usr/local/bin/yarn` 软链，属于改配置，本次不做。
- 结论：出现"要不要用 yarn"时答案固定 —— 用 npm；只在遇到别人的 `yarn.lock` 项目时才需要它，那种情况优先 `npm install` 后自查依赖树。
