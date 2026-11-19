# DESIGN.md 自产流程（检查报告 vs 风格参照）
## 一句话
两套东西都叫 DESIGN.md，但一个是**给本项目立法**（impeccable `document` 从代码反推、tokens 是规范），一个是**抄别人的法**（design-md 库 84 份真实站点分析，只是参照素材）。混用会直接污染下一次体检。

## 实际效果（实测 2026-10-26）
- **自产（impeccable 侧）**
  - `document`：从现有代码反推本项目 DESIGN.md；遵循 [google-labs-code/design.md spec]——YAML frontmatter 装 tokens（`colors/typography/rounded/spacing/components`，其中 `components` 用 `{colors.primary}` 这类引用），正文最多 8 节固定顺序：`Overview / Colors / Typography / Layout / Elevation & Depth / Shapes / Components / Do's and Don'ts`（无关小节应删掉而不是编造）
  - `init`：只写 `PRODUCT.md`（产品事实），**明确不写 DESIGN.md**；`extract`：把可复用 tokens/组件收进设计系统。CLI 侧**没有** `document` 命令（实测 → `Unknown command`），全靠模型读 `reference/document.md` 干活
  - 产出的 DESIGN.md 会被引擎反向消费：实测在 `/tmp/dstest` 放一份带 `colors.primary/rounded.md` 的 DESIGN.md，`detect p.html` 立刻多出 `design-system-color`（"background rgb(255,0,0) … outside DESIGN.md"）与 `design-system-radius` 两条 **advisory**；加 `--no-design-system` 后两条消失
- **参照（design-md 库）**
  - `/data/workspace/design-md-library/design-md/` 84 个品牌，每份 `DESIGN.md`（4–44KB）+ 218B README，**无截图、无 tokens.json**；frontmatter 是 `version: alpha`，性质写明 "inspired interpretation"（stripe 那份连名字都是 `Stripi-Inspired-design-analysis`）。`/design <品牌>` 只把 frontmatter 1800 字 + `## Overview` 起 4800 字喂模型 → 组件/间距细节不进上下文
- **边界**（当前文档没人写的那条）：

| | 自产（impeccable document） | 参照（design-md 库） |
|---|---|---|
| 输入 | **本项目**源码/CSS/组件 | 别人的线上站点 |
| 产物 | 本项目 DESIGN.md（tokens=规范） | 只读素材，不该进项目 |
| 用途 | 让后续 detect 有 drift 基准；让 AI 生成新页面不跑偏 | 定风格方向、抄 tokens 取值、学铁律 |
| 可否改 | 是（它就是本项目的法） | **否**（改了就不是那家品牌了） |
| 落盘位置 | 项目根 `DESIGN.md` | 留在 `/data/workspace/design-md-library/` |

## 怎么调
```bash
# 自产：让 pi 按 impeccable 的 document 流程产出（需要模型，不是 CLI）
/skill:impeccable   # 然后说"document 这个项目"，它会读 reference/document.md 走 scan 模式
grep -n "^## " DESIGN.md            # 应为 8 节规范名的子集，顺序不乱
sed -n '1,30p' DESIGN.md            # frontmatter 必须能被 YAML 解析
/root/.pi/agent/skills/impeccable/scripts/impeccable context   # designPath 应指向它
# 参照：先读摘要再读原文
grep -n "stripe" /data/workspace/design-md-library/DESIGN-MD-INDEX.md
sed -n '1,80p' /data/workspace/design-md-library/design-md/stripe/DESIGN.md
```

## 坑
- **别把库里的 DESIGN.md 直接复制进项目根**：它带的是别家品牌名和别家 tokens，之后每次 `detect` 都会拿它当基准 → 一片假 `design-system-*` advisory，真 drift 反而被噪声淹掉。
- `design-system-*` 规则是 **advisory**：永不改变退出码、永不计入失败数，所以"体检通过"不代表风格一致。
- URL 目标的 project root 落在 `cwd`（网关里是 `/data/workspace`），那里**没有** DESIGN.md → 扫线上站点时 design-system 比对永远不生效。想让 URL 也比对，得把 DESIGN.md 放到网关 cwd 或改 `run()` 的 cwd。
- `init` ≠ `document`：`init` 只写 PRODUCT.md，指望它顺手生成 DESIGN.md 会白等。

> 品牌库本体、索引字段坑、`/design` 路由匹配逻辑 → `03-SEO与内容/D-design-md.md`，此处不重复。
