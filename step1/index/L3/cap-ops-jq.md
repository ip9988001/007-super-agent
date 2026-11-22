# cap-ops-jq

## 一句话
命令行 JSON 切片的默认工具（容器内已装，不用再拿 `python -m json.tool` 顶）。

## 怎么调
jq -c '.a.b|add' # 紧凑输出 + 聚合 jq -r '.items[].name' # 裸字符串（无引号，可进 shell 管道） jq '.[] | select(.score>0.8)' # 过滤 jq -s '.' # slurp：把多行 JSON 合成数组 curl -s URL | jq -r '.data[].id' # 抓取链标配（见 01-取数与抓取/C-curl-jq.md）

## 坑
1.6 是 Debian 12 的老版本：没有 `--stream` 的部分新特性、`@csv` 对 null 的处理与 1.7 不同；大文件（>100MB JSON）jq 是全量载入内存，容易 OOM —— 这种用 `jq --stream` 或换 python 流式。

## 实际效果
`jq --version` = `jq-1.6`。；实测：`echo '{"a":{"b":[1,2,3]}}' | jq -c '.a.b|add'` → `6`，退出码 0。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`jq --version` = `jq-1.6`。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/J-jq.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：jq-1.6
