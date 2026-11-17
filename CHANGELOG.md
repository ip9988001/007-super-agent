# Changelog

本文件记录 007-super-agent 的版本变更，格式参考 Keep a Changelog，版本号遵循语义化版本。

## [Unreleased]

### Added
- 第 19 步第 5 批：注册表 73 → 84 条（+4 CLI：tesseract/magick/mpv/npm；+8 助手脚本条目；+1 /audit 入口 run-audit.sh）；install-toolchain.sh（公开源可重复安装脚本，版本钉死）。
- 第 19 步第 5 批：探针按 call_type 分流（cli 查命令/路径、service 查脚本文件、doc/sop/tool/mcp/data 不探针）；L2 ops 类拆成 ops.md + ops-2.md（每份 ≤25 行）。
- 第 13 步：分诊提示词带 72 条 compact 工具清单（冻结区）；费用按任务归属（task_id）；监工调用记费用；启动体检一行汇总；监工预审证据包（目标/参数核验）。
- 第 14 步（P0）：网关首封 3 秒防抖；inbox_queue 持久化队列 + (task_id, inbound_seq) 防重放；未验证工具先探针；水位超时自动重排 1 次；账本每日 03:30 热备；Fast(2)/Heavy(1) 双通道。
- 第 15 步：记忆自动备份（04:00 精炼后推私密库 memories/<server_id>/，默认关）。

### Fixed
- 第 14 步实测抓出的 fastSem 泄漏（第三条任务起不再处理）已修，加 TestSequentialThree 回归测试。
- 第 15 步验证抓出的 askpass 执行位与 remote userinfo 解析问题已修。

## [v0.1.0] - 2026-11-17

### Added
- Go 单文件骨架：`cmd/super-agent/main.go` + `go.mod`（module `007-super-agent`，go 1.22）。
- native direct mode：单静态二进制原生部署，无沙箱、无白名单，权限完整直通执行。
- build 脚本与环境说明随骨架一同入库，`go build ./cmd/super-agent` 即可产出可执行文件。
