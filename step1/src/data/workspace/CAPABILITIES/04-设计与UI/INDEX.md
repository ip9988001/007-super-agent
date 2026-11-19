# 04 设计与UI · 条目索引
> 挑一条，只读那个 md。组内按文件名首字母排序，组之间空一行。

## ① 设计体检引擎
- I-impeccable.md :: detect 二进制 0.1.16（静态 0.06s / URL 2–29s，实测触发 22 个规则 id；「62 条」未获证实）
- I-impeccable-skill.md :: SKILL.md 24 命令提示词库（**CLI 只实现 detect**，其余靠模型读 reference）

## ② 网关路由（体检与写盘）
- G-design-write.md :: `/design --write` 落盘分支（备份全靠 prompt，无网关兜底，覆盖 DESIGN.md 无提示）
- G-ui-route.md :: `/ui` 三段链路 detect → 聚合 → pi（quick 150+180s / deep 240+420s，`--deep` 不换规则）

## ③ 设计语言与视觉验证
- B-ui-visual.md :: 截图 / 视觉回归姿势（chrome-nosandbox 0.76s / Playwright 多视口，实现细节指向 01/B）
- D-design-md-自产.md :: DESIGN.md 自产（impeccable `document`，给本项目立法）vs 库参照（抄别人，只读）

## ④ 网关超时核查
- G-gateway-timeouts.md :: 全路由超时矩阵 + `/stats` 20s 单点故障注入验证（-9 / 输出截断）

> 本批实测**纠正/缺件**：**不存在 `/status` 路由**（只有 `/help /audit /stats /graph /rea /seo /ui /design` + 默认兜底）· 「62 条确定性规则」在任何发布文件里都查不到，是 `bot-gateway.js` 自填 · `impeccable critique/polish/document` 等 23 个命令**没有 CLI 实现**。
> 品牌库本体与 `/design` 匹配逻辑见 `03-SEO与内容/D-design-md.md`；浏览器栈见 `01-取数与抓取/B-browser-stack.md`，此处不重复。
