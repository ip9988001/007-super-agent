# Changelog

本文件记录 007-super-agent 的版本变更，格式参考 Keep a Changelog，版本号遵循语义化版本。

## [v0.1.0] - 2026-11-17

### Added
- Go 单文件骨架：`cmd/super-agent/main.go` + `go.mod`（module `007-super-agent`，go 1.22）。
- native direct mode：单静态二进制原生部署，无沙箱、无白名单，权限完整直通执行。
- build 脚本与环境说明随骨架一同入库，`go build ./cmd/super-agent` 即可产出可执行文件。
