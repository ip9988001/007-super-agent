# jq 1.6

> **一句话**：命令行 JSON 切片的默认工具（容器内已装，不用再拿 `python -m json.tool` 顶）。

## 实际效果（实测）
- `jq --version` = `jq-1.6`。
- 实测：`echo '{"a":{"b":[1,2,3]}}' | jq -c '.a.b|add'` → `6`，退出码 0。
- 是 `start.sh` 1 段基础包自愈的**校验项之一**（`! command -v jq` 就触发整段 apt 重装）。

## 怎么调
```
jq -c '.a.b|add'                 # 紧凑输出 + 聚合
jq -r '.items[].name'            # 裸字符串（无引号，可进 shell 管道）
jq '.[] | select(.score>0.8)'    # 过滤
jq -s '.'                        # slurp：把多行 JSON 合成数组
curl -s URL | jq -r '.data[].id' # 抓取链标配（见 01-取数与抓取/C-curl-jq.md）
```

## 坑
- 1.6 是 Debian 12 的老版本：**没有** `--stream` 的部分新特性、`@csv` 对 null 的处理与 1.7 不同；
  复杂表达式在 1.7 上能跑的这里可能报语法错。
- 大文件（>100MB JSON）jq 是**全量载入内存**，容易 OOM —— 这种用 `jq --stream` 或换 python 流式。
- 非 UTF-8 输入需 `jq -a`（ASCII 输出）或先 `iconv`。
- 和 `01-取数与抓取/C-curl-jq.md` 的差异：那条写的是「jq 缺失，用 python 顶」的旧状态，**本机已有 jq-1.6**，以本条为准。
