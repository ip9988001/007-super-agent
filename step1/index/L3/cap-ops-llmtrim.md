# cap-ops-llmtrim

## 一句话
本地 MITM 压缩代理（`127.0.0.1:43117`），改写发往各家 LLM API 的请求体以少付 input token。

## 怎么调
/data/bin/llmtrim-health.sh # 判活正解：端口可连 + CONNECT 端到端，OK/DOWN + exit 0/1 llmtrim _alive; echo $? # 语义判活：exit 0 curl -s -o /dev/null -w '%{http_connect}\n' -x http://127.0.0.1:43117 https://example.com/ # 期望 200 llmtrim status | head -3 # 看 running / 省了多少 llmtrim start|stop # 起停（start.sh 3b 段每次重建都会 start） export HTTPS_PROXY=http://127.0.0.1:43117 NODE_EXTRA_CA_CERTS=/data/state/llmtrim/ca.pem

## 坑
见 registry.json 的 note 字段。

## 实际效果
进程活着：`PID 84`，`llmtrim serve --port 43117 --supervised`（PPid=1，v0.13.4，`restarts:0`），`up 10h41m`。；资源：`VmRSS 89MB` 恒定、9 线程、累计 CPU `7.9s/10h41m`（≈0.02%），无空转。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：进程活着：`PID 84`，`llmtrim serve --port 43117 --supervised`（PPid=1，v0.13.4，`restarts:0`），`up…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/L-llmtrim.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：实测省 5.43%，11-01 后 16 天零新增；改历史消息会击穿 DeepSeek 前缀缓存，新 Agent 不用
