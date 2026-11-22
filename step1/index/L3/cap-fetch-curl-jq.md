# cap-fetch-curl-jq

## 一句话
最底层的 HTTP 取数：curl 7.88.1 可用；jq 1.6 已装（见 05/J-jq.md），可直接 jq 管道；python3 仅作复杂处理备选。

## 怎么调
# 拿 JSON 并取字段（jq 的替代） curl -s -H "Accept: application/json" "https://api.github.com/repos/torvalds/linux" \ | python3 -c "import sys,json;d=json.load(sys.stdin);print(d['stargazers_count'],d['full_name'])" # 漂亮打印 / 过滤键（等价 jq . / jq .a.b） curl -s URL | python3 -m json.tool | head -40 curl -s URL | python3 -c "import sys,json;print(json.load(sys.stdin)['key'])" # 只取状态码 + 计时，判活 curl -s -o /dev/null…

## 坑
任何脚本里写 `| jq` 都会直接失败（exit 127），别抄网上带 jq 的模板。；`https://api.github.com` 未带 token 限 60 次/小时，批量先想别的路（agent-reach github / gh 带 token）。

## 实际效果
`curl -s -o /dev/null -w "%{http_code} %{time_total}s" https://api.github.com/zen` → `200 0.117s` ✅；`curl -s https://api.github.com/zen` → 返回一句话文本 ✅

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`curl -s -o /dev/null -w "%{http_code} %{time_total}s" https://api.github.com/zen` → `200…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/C-curl-jq.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：jq 1.6 已装；r.jina.ai 免 key 转 markdown
