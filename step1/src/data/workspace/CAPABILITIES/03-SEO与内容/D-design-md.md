# design-md-library
## 一句话
84 个真实站点设计语言的 DESIGN.md（每份 4–44KB，VoltAgent/awesome-design-md · MIT）；`/design` 路由按品牌取 tokens + 铁律，或按它改造目标项目。

## 实际效果（实测 2026-10-26）
- `design-md/` 下 84 个品牌目录，**每个都有 DESIGN.md，零缺失**；与 `design-md-index.json` 完全对齐（84 vs 84，无孤儿、无缺项）✅
- `DESIGN-MD-INDEX.md` = 1 表头 + 84 行（品牌/大小/主色/字体/半径）✅
- **路由逻辑离线复跑**（照抄 `bot-gateway.js:553-585`，精确→子串两级匹配）：`stripe`→stripe(24.6KB) `claude`→claude(33.6KB) `apple`→apple(37KB) `figma`→figma `nike`→nike `vercel`→vercel `linear`→linear.app `line`→linear.app ✅；`openai` / `xiaomi` → MISS（库里确实没有）
- 品牌目录只有两个文件：`DESIGN.md` + 218B `README.md`。**没有截图、没有 tokens.json、没有组件代码**
- DESIGN.md 是真 frontmatter：`colors.*`（20+ 命名色值，如 claude 的 `primary:#cc785c` / `canvas:#faf9f5`）+ `typography.*`（fontFamily / fontSize / fontWeight / lineHeight / letterSpacing）→ 可直接转 CSS 变量
- 性质是 **"inspired interpretation"**（受该站启发的再分析，非官方规范）；stripe 那份 frontmatter 原文就写 `name: Stripi-Inspired-design-analysis`（拼写如此）

## 怎么调
```bash
L=/data/workspace/design-md-library
ls $L/design-md | wc -l                                  # 84
grep -n "stripe" $L/DESIGN-MD-INDEX.md                  # 先看一行摘要再决定读不读
sed -n '1,80p' $L/design-md/stripe/DESIGN.md            # frontmatter tokens（最值钱的一段）
grep -n "^## " $L/design-md/stripe/DESIGN.md            # Overview/Components/Shapes… 按需跳读
python3 -c "import json;d=json.load(open('$L/design-md-index.json'));print([x['brand'] for x in d][:10])"
# TG 侧：/design list · /design stripe · /design stripe <项目路径> [--write]
```

## 坑
- 索引的「字体」列被硬截在 72 字符（airbnb 收在 "…display, body, n"），`airbnb` / `cohere` / `kraken` 的「半径」为空 —— **索引只能当目录，不能当设计依据**，要原文。
- 子串匹配有歧义风险：`line` 命中 `linear.app`；想找 openai 会 MISS（库里近邻是 `x.ai` / `together.ai` / `claude` / `cohere`），MISS 时只回 8 个相近名。
- 路由只截 frontmatter 1800 字 + `## Overview` 起 4800 字喂模型 —— 组件/间距/Elevation 那几段不进上下文，要细节得自己读文件。
- `--write` 会真改目标源码（先 `.bak`）+ 把 DESIGN.md 复制进项目根；不加 `--write` 是只读，只出代码块。
- 与 impeccable 是两条路：本库给"参照样本"，impeccable 给"62 条校验规则"（属 04，未建前读 `/root/.pi/agent/skills/impeccable/SKILL.md`），别重复读同一批内容。
