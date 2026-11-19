# 007 部署说明（systemd 版）

> 适用：007-super-agent 仓库，Debian 12，root。生产机 VM124655 已按此接管
> （2026-11-19，旧 nohup PID 2795463 → systemd PID 2823266，重启复测通过）。

## 首次部署（新服务器）

私密仓库 super-agent-007 里有完整清单与一键脚本：`install/TOOLS-INVENTORY.md`
与 `install/install-007.sh`。按清单顺序走：

1. 克隆公开库与私密库。
2. 跑 `deploy/deps.sh`（系统依赖 + Go + Ollama + 完整工具链）。
3. `go build -o super-agent ./cmd/super-agent`，把二进制放到 `/root/super-agent-007/`。
4. 私密库 `scripts/seed-tools.sh` 恢复工具种子，`scripts/apply-secrets.sh`
   落 DeepSeek 密钥到 `/etc/super-agent/.env`（0600），其余键按
   `install/super-agent.env.example` 补齐（GitHub token、Tavily 等不进仓库）。
5. 装 unit：`cp deploy/super-agent.service /etc/systemd/system/`，
   `systemctl daemon-reload && systemctl enable --now super-agent`。
6. 验证：`systemctl is-active super-agent`、`ss -ltnp | grep 6262`、
   `journalctl -u super-agent | grep "registry ready"`、`/usr/local/bin/go version`。

## 日常再部署（新 step）

不再用 nohup，一律走 systemd：

```sh
systemctl stop super-agent
cp data/registry/registry.json data/registry/backup-<step>-<date>/registry.json
cp super-agent super-agent.bak-<step>
cp /tmp/<step>/super-agent ./super-agent
systemctl start super-agent
# 校验：systemctl is-active / 新 PID / 6262 / journalctl registry ready
```

模板见本地 `sa007-redeploy-systemd.sh`（传包、备份、校验套路与 step2022t 相同，
只把启动部分换成 systemd）。

## 回滚

unit 起不来时：`systemctl stop super-agent`，用备份的二进制恢复后
`systemctl start super-agent`；还不行就用老办法临时拉起：

```sh
export CHROME_PATH=/root/super-agent-007/tools/scripts/chrome-with-nosandbox
nohup ./super-agent > /tmp/sa007-rollback.log 2>&1 &
```

## 已知事项

- 重启后自启（reboot survival）未在生产机实测（不能重启生产机，xray/nginx
  在跑）。unit 已 enable，理论上保证，下一次虚拟机替换或重启时再验证。
- 007 只监听 127.0.0.1:6262，不对外暴露。
- 密钥只在 `/etc/super-agent/.env`（0600），绝不进公开库。
