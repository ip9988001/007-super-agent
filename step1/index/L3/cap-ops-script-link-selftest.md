# cap-ops-script-link-selftest

## 一句话
link 中继自测（17 行 node）：对三号的 link 中继发 6 组探测（正确 token / 重放 nonce / 错误 token / 过期 ts / 错 from / 无认证头），输出状态码对照表。

## 怎么调
node tools/scripts/link-selftest.cjs # 仅三号可用，007 上不要跑

## 坑
三号专属：`require('/data/bin/totp.js')`、读 `/data/state/link/link.env`、调 `lang.xyz777.cc.cd` 中继。007 上无 link 服务、无 link.env，直接跑必失败；本条只存档、不调用。

## 实际效果
文件在种子内（17 行）；007 上不可用（缺 link.env 与中继），已在注册表外说明。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：本条在 007 上不调用；三号实测输出为 6 组状态码 JSON；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：tools-seed/scripts/link-selftest.cjs · 核实：2026-11-18（第 19 步种子核验）
