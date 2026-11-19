# curl + jq
## 一句话
最底层的 HTTP 取数：curl 7.88.1 可用；**jq 1.6 已装**（见 05/J-jq.md），可直接 jq 管道；python3 仅作复杂处理备选。

## 实际效果（实测 2026-10-26）
- `curl -s -o /dev/null -w "%{http_code} %{time_total}s" https://api.github.com/zen` → `200 0.117s` ✅
- `curl -s https://api.github.com/zen` → 返回一句话文本 ✅
- `jq --version` → `jq-1.6` ✅（早期记录作废：当时确实没装，后续已安装）
- `curl ... | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['stargazers_count'])"` → `253527` ✅
- `curl -s https://r.jina.ai/https://example.com` → ✅ 免 Key 的网页转 markdown 通道（curl 直连就能用）
- wget / aria2c / httpie / xh / pup / htmlq / pandoc 全部 MISSING（别指望）

## 怎么调
```bash
# 拿 JSON 并取字段（jq 的替代）
curl -s -H "Accept: application/json" "https://api.github.com/repos/torvalds/linux" \
  | python3 -c "import sys,json;d=json.load(sys.stdin);print(d['stargazers_count'],d['full_name'])"

# 漂亮打印 / 过滤键（等价 jq . / jq .a.b）
curl -s URL | python3 -m json.tool | head -40
curl -s URL | python3 -c "import sys,json;print(json.load(sys.stdin)['key'])"

# 只取状态码 + 计时，判活
curl -s -o /dev/null -w "%{http_code} %{time_total}s\n" URL

# 网页转干净 markdown（免 key，jina reader）
curl -s "https://r.jina.ai/https://target-page" | head -60

# 带 UA / cookie / 跟随跳转
curl -sL -A "Mozilla/5.0" -b "k=v" -H "Referer: https://x.com/" URL
```

## 坑
- 任何脚本里写 `| jq` 都会直接失败（exit 127），别抄网上带 jq 的模板。
- `https://api.github.com` 未带 token 限 60 次/小时，批量先想别的路（agent-reach github / gh 带 token）。
- 默认不跟跳转（要 `-L`）；很多站点无 UA 直接 403。
- 返回是空壳（只有骨架屏/LOADING）说明是 JS 页，别再调 curl，升级到 lightpanda / scrapling。
- r.jina.ai 对长文会截断，需全文用 `scrapling extract get`。
