# 03 SEO与内容 · 条目索引
> 挑一条，只读那个 md。组内按文件名首字母排序，组之间空一行。

## ① SEO 入口与技能库
- S-claude-seo.md :: 26 skill + 19 agent + 60 脚本技能库 v2.5.0（托管 venv 771MB；启动器已被 start.sh 穿透软链覆盖成自转发，须绕行 `runtime.py`）
- S-seo-gateway.md :: `/seo` 路由链 audit.py → prompt → pi（quick 180s / deep 420s，deep 名不副实；doctor 那步空转 180s）

## ② 确定性体检
- G-geo-seo-aeo.md :: geo `audit.py` 零依赖 JSON 体检（无总分，口径在 scoring.md；**不处理 gzip，压缩站全零**）

## ③ 内容打分与 AI 可读性
- C-content-scorers.md :: 内容三件套 content_quality（0–100 分）/ content_humanize（去 AI 腔）/ content_verify（引用缺口）
- L-llms-txt.md :: llms.txt 模板 + 检测（**无生成器**，靠 prompt 现写）

## ④ 设计语言库
- D-design-md.md :: 84 个品牌 DESIGN.md（tokens 真值，`/design` 按品牌名两级匹配）

## ⑤ 索引本体
- T-toolkit-index.md :: `seo-toolkit/TOOLKIT-INDEX.md` 新鲜度逐条核对（路由表准 / 入口命令失效）

> 本批实测**失效或缺件**（未写空壳 md）：`scripts/claude-seo` 启动器（git HEAD 版可用）· `/seo --deep` 的 6 规程串行 · llms.txt 生成器 · DataForSEO MCP · GSC/Google 凭据（故 seo-google / seo-maps / seo-dataforseo / seo-backlinks 只能读规程，出不了实时数据）。
> 浏览器渲染需求见 `01-取数与抓取/B-browser-stack.md`，此处不重复。
