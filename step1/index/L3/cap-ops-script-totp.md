# cap-ops-script-totp

## 一句话
TOTP 库（85 行 node）：xinxin-link 的 TOTP 算法，60 秒一步、位数 5/6/7 随步随机、有效 3 分钟；零依赖，只用 node 内置 crypto。

## 怎么调
node -e "const t=require('./tools/scripts/totp.js'); console.log(t.current('SECRET', Math.floor(Date.now()/1000)))" # 须传 secret

## 坑
它是库不是 CLI：直接 `node tools/scripts/totp.js` 什么都不输出；须 `require` 后调 `current(secret, unix秒)`；secret 来自 link.env，007 上没有——只在有 secret 的场景下用。

## 实际效果
文件在种子内（85 行）；`require` 可加载（纯 crypto，无外部依赖）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`current()` 返回 {code, digits}（同分钟同 secret 结果一致）；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：tools-seed/scripts/totp.js · 核实：2026-11-18（第 19 步种子核验）
