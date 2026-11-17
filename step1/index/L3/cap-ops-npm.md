# cap-ops-npm

## 一句话
npm 10.9.9（随 Node 22.23.4 官方包）：Node 的包管理器；全局工具（rea、codegraph、playwright）都经它装。

## 怎么调
npm --version # 版本
npm install -g <pkg>@<ver> # 全局装，版本钉死
npm list -g --depth=0 # 看已装全局包

## 坑
别用 `npm install -g` 装需要编译的原生模块（无头机缺构建链会失败）；全局包装到 `/usr/local/lib/node_modules`，`npm list -g` 才能看见；`npx <pkg>` 会先联网找最新版，离线/钉死版本时用已装的全局命令，别用 npx。

## 实际效果
`npm --version` → `10.9.9` ✅；`node --version` → `v22.23.4` ✅（第 19 步官方包实测）；全局包 rea 6.8.0、codegraph 1.6.2 已装。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`npm --version` → `10.9.9` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：第 19 步安装核验（--version 实测，2026-11-18）
