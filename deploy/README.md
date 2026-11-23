# 007 部署说明（systemd 版）

> 适用：007-super-agent 仓库，Debian 12，root。生产机 VM124655 已按此接管。
> 2026-11-24 起：Python 工具全走独立 venv（/opt/007-venv），系统 Python 不动。

## 一键部署（新服务器，从零到投产）

```sh
git clone https://github.com/ip9988001/007-super-agent.git /root/007-super-agent
cd /root/007-super-agent
sudo bash deploy/install.sh
```

`deploy/install.sh` 一次做完：系统依赖 → Python venv → 编译二进制
（主程序 + ctxread）→ 数据目录（宪法/注册表/索引）→ 工具目录 →
DeepSeek 密钥 → systemd 守护 → 每周漏洞巡检定时器 → 验证。
密钥会交互式问你要（或用 `DEEPSEEK_API_KEY=sk-...` 传进来），只存进
`/etc/super-agent/.env`（0600），绝不进仓库、不打印。

部署完就投产：服务监听 127.0.0.1:6262，POST /wake 收任务。

## 各步骤单独跑

| 步骤 | 命令 | 说明 |
| --- | --- | --- |
| 系统依赖 + 工具链 + venv | `sudo bash deploy/deps.sh` | apt、Go 1.27.4、Ollama、完整工具链、/opt/007-venv |
| 只建 venv | `sudo bash deploy/build-venv.sh` | 已建好且健康就跳过；`--rebuild` 强制重建 |
| 只编译 | `go build -o /root/super-agent-007/super-agent ./cmd/super-agent` | 再加 `go build -o /root/super-agent-007/tools/ctxread ./cmd/ctxread` |
| 只装服务 | `cp deploy/super-agent.service /etc/systemd/system/ && systemctl daemon-reload && systemctl enable --now super-agent` | 服务文件已含 venv 环境变量 |
| 只装巡检定时器 | 见 `ops/security/` | 每周日凌晨 3 点扫漏洞 |

## 部署结构（新服务器上长这样）

```
/root/007-super-agent/          # 公开库（代码 + 部署脚本）
/root/super-agent-007/          # 运行目录（WorkingDirectory）
  super-agent                  # 主二进制
  tools/ctxread                # 压缩读工具
  tools/skills、seo-toolkit、scripts
  data/prompts/                # 三本宪法（front/executor/advisor）
  data/registry/registry.json  # 90 个工具注册表
  data/index/                  # L1/L2/L3 索引
  data/ledger.db               # 花费账本
/opt/007-venv/                 # 独立 Python 环境（007 的 Python 工具全走这里）
/etc/super-agent/.env          # 密钥（0600）：DEEPSEEK_API_KEY 等
/etc/systemd/system/super-agent.service
/etc/systemd/system/007-vuln-audit.{service,timer}   # 每周漏洞巡检
```

## 日常再部署（新版本）

```sh
systemctl stop super-agent
cp /root/super-agent-007/super-agent /root/super-agent-007/super-agent.bak-$(date +%Y%m%d)
# 在仓库里改代码、编译
cd /root/007-super-agent && go build -o /root/super-agent-007/super-agent ./cmd/super-agent
systemctl start super-agent
# 校验：systemctl is-active / 新 PID / 6262 / journalctl
```

## 回滚

- 二进制出问题：`systemctl stop super-agent`，用 `.bak-日期` 的备份恢复后 `systemctl start super-agent`。
- venv 出问题：删掉两行 Environment（VIRTUAL_ENV、PATH）重启即回系统 Python；或 `sudo bash deploy/build-venv.sh --rebuild` 重建 venv。
- 服务起不来时的临时拉起：
  ```sh
  export CHROME_PATH=/root/super-agent-007/tools/scripts/chrome-with-nosandbox
  export VIRTUAL_ENV=/opt/007-venv PATH=/opt/007-venv/bin:$PATH
  nohup ./super-agent > /tmp/sa007-rollback.log 2>&1 &
  ```

## 已知事项

- 重启后自启（reboot survival）未在生产机实测（不能重启生产机，xray/nginx 在跑）。unit 已 enable，理论上保证，下一次虚拟机替换或重启时再验证。
- 007 只监听 127.0.0.1:6262，不对外暴露。
- 密钥只在 `/etc/super-agent/.env`（0600），绝不进公开库。
- Python 依赖在 venv 里（/opt/007-venv），系统 Python（apt 包）永远不动；每周巡检只扫 venv，不扫系统 Python。
- soupsieve 2.10 有 3 个暂无可装修复版的漏洞（修复版要求 Python >=3.11.5，本机 3.11.2），已进豁免清单 `ops/security/vuln_ignore.json`（过期日 2027-02-24）。
