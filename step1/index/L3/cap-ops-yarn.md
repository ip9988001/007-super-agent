# cap-ops-yarn

## 一句话
yarn 1.22.22（classic 1.x，不是 berry 2+）已经装好，和 npm 10.9.9 共用同一个 node v22.23.4；本机/本项目不用它，一律 npm。

## 怎么调
npm -v && node -v # 需要确认运行时版本，用这两条 npm ls -g --depth=0 # 看全局装了什么

## 坑
装了 ≠ 该用：yarn 1.x 与 npm 是两套 lockfile + 两套全局目录，混用会出现"npm 装完 yarn 找不到"。；yarn 1.22 已停止演进，新项目要么 npm 要么 pnpm/yarn berry；本机没有 berry。

## 实际效果
`yarn --version` → `1.22.22` ✅；`npm -v` → `10.9.9`；`node -v` → `v22.23.4`（同一运行时，yarn 只是另一个包管理器前端）

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`yarn --version` → `1.22.22` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/Y-yarn.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：1.22.22 classic 1.x，不是 berry 2+
