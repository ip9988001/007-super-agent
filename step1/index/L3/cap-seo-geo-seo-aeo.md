# cap-seo-geo-seo-aeo

## 一句话
纯标准库的确定性 SEO/GEO/AEO 体检器：抓一页 → 输出 JSON 事实（布尔+计数，不含总分），零依赖、秒级、可复现。

## 怎么调
cd tools/seo-toolkit/geo-seo-aeo-skill python3 scripts/audit.py https://target/page # 直连（压缩站点会失真，见坑） curl -sS --compressed -A "Mozilla/5.0" <url> -o /tmp/p.html # 唯一可靠姿势：先落地 python3 scripts/audit.py --file /tmp/p.html --base-url https://target python3 scripts/audit.py --file p.html --base-url https://t/ | python3 -m json.tool # 只取某一维 python3 scripts/audit.py <url> | python3 -c "import j…

## 坑
❌ 完全不处理 Content-Encoding：`fetch()` 只 `resp.read().decode(charset)`，gzip 字节直接喂 HTMLParser。python.org 直连 → titl…；结论固定：目标站开 gzip/brotli 时，直连 URL 的数全废。先 `curl --compressed` 落地再 `--file`（实测这样才出真值）。

## 实际效果
`python3 scripts/audit.py https://example.com` → 完整 JSON ✅ 覆盖：title(+长度/ok)、meta_description(+长度/ok)、canonica…；有判别力（自带 fixture，同参数两次跑结果一致）

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /seo；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`python3 scripts/audit.py https://example.com` → 完整 JSON ✅ 覆盖：title(+长度/ok)、meta_descript…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/03-SEO与内容/G-geo-seo-aeo.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：纯标准库确定性体检，输出 JSON 事实（布尔+计数，不含总分）
