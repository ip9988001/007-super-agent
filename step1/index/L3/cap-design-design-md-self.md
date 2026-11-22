# cap-design-design-md-self

## 一句话
两套东西都叫 DESIGN.md，但一个是给本项目立法（impeccable `document` 从代码反推、tokens 是规范），一个是抄别人的法（design-md 库 84 份真实站点分析，只是参照素材）。混用会直接污染下一次体检。

## 怎么调
# 自产：让 pi 按 impeccable 的 document 流程产出（需要模型，不是 CLI） /skill:impeccable # 然后说"document 这个项目"，它会读 reference/document.md 走 scan 模式 grep -n "^## " DESIGN.md # 应为 8 节规范名的子集，顺序不乱 sed -n '1,30p' DESIGN.md # frontmatter 必须能被 YAML 解析 /root/.pi/agent/skills/impeccable/scripts/impeccable context # designPath 应指向它 # 参照：先读摘要再读原文 grep -n "stripe" /data/workspace/design-md-library/DESIGN-MD-INDEX.md sed -n '1,80…

## 坑
别把库里的 DESIGN.md 直接复制进项目根：它带的是别家品牌名和别家 tokens，之后每次 `detect` 都会拿它当基准 → 一片假 `design-system-*` advisory，真 drift 反…；`design-system-*` 规则是 advisory：永不改变退出码、永不计入失败数，所以"体检通过"不代表风格一致。

## 实际效果
自产（impeccable 侧）；`document`：从现有代码反推本项目 DESIGN.md；遵循 [google-labs-code/design.md spec]——YAML frontmatter 装 tokens（`colors/typog…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /design；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：自产（impeccable 侧）；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/04-设计与UI/D-design-md-自产.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：检查报告 vs 风格参照；给本项目立法 vs 抄别人的法是两回事
