# geo audit.py
## 一句话
纯标准库的确定性 SEO/GEO/AEO 体检器：抓一页 → 输出 JSON 事实（布尔+计数，不含总分），零依赖、秒级、可复现。

## 实际效果（实测 2026-10-26）
- `python3 scripts/audit.py https://example.com` → 完整 JSON ✅ 覆盖：title(+长度/ok)、meta_description(+长度/ok)、canonical、robots_meta、h1_count、heading_levels_skipped、images / images_missing_alt、内链外链数、og_tags、twitter_tags、word_count、hedge_words_found、quantified_signal_count、has_tables、llms.txt 存在性、FAQ/HowTo schema、JSON-LD 校验、完整 headings 树
- **有判别力**（自带 fixture，同参数两次跑结果一致）：
  - `tests/weak-page.html` → title_ok=false · meta_ok=false · jsonld 0/0 · og=0 · 51 词 / 11 个模糊词 / 量化 0
  - `tests/strong-page.html` → title_ok=true · meta_ok=true · jsonld 1/1 · og=1 · 92 词 / 0 模糊词 / 量化 13 / 有表 / FAQ ✅
- **不出总分**：`references/scoring.md` 才是口径（SEO/GEO/AEO 各 0–100，P0 阻断→P3 打磨，逐条扣分），总分要 agent 按它折算。想要一个数字就自己乘权重。
- 真站实跑 python.org：title 21 字符→false、meta 52→false、h1_count=5、内链 128 / 外链 80、og 7、jsonld 1×`WebSite`、1011 词、has_tables=true（数据见下方坑 1，这是 `--file` 路径跑出来的）

## 怎么调
```bash
cd /data/workspace/seo-toolkit/geo-seo-aeo-skill
python3 scripts/audit.py https://target/page                          # 直连（压缩站点会失真，见坑）
curl -sS --compressed -A "Mozilla/5.0" <url> -o /tmp/p.html           # 唯一可靠姿势：先落地
python3 scripts/audit.py --file /tmp/p.html --base-url https://target
python3 scripts/audit.py --file p.html --base-url https://t/ | python3 -m json.tool
# 只取某一维
python3 scripts/audit.py <url> | python3 -c "import json,sys;d=json.load(sys.stdin);print(d['geo'])"
```

## 坑
- ❌ **完全不处理 Content-Encoding**：`fetch()` 只 `resp.read().decode(charset)`，gzip 字节直接喂 HTMLParser。python.org 直连 → title/meta/h1/links 全 null/0，word_count 反被二进制噪声灌成 **1928 假数字**。example.com 不压缩所以看着正常。
- 结论固定：**目标站开 gzip/brotli 时，直连 URL 的数全废**。先 `curl --compressed` 落地再 `--file`（实测这样才出真值）。
- `llms_txt` 是对 `scheme://host/llms.txt` 再发一次请求（10s 超时，跟重定向）；`--file` 不带 `--base-url` 就恒为 false，别当"没有 llms.txt"的结论。
- hedge 词表是硬编码 27 个英文词（scalable / robust / seamless / cutting-edge…），中文内容永远 0 分，别当质量结论。
- 计数基于原始 HTML：SPA 空壳页词数接近 0（要渲染走 `01/B-browser-stack.md`）。
- 无鉴权、无 UA 列表轮换，被 Cloudflare 拦会直接抛 URLError → 网关那侧就退化成"未取得 JSON"。
