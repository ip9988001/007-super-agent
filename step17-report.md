# 第 17 步报告：DeepSeek 密钥预配置进私密库（2026-11-18 00:41–00:50）

## 做了什么

- 私密库 `super-agent-007`：`0af477a..7d50341`，新 HEAD `7d50341`。
  - `secrets/deepseek.env`：本地 `~/.config/super-agent-007/deepseek.env` 的完整内容
    （密钥 + deepseek-flash / deepseek-reasoner / STANDARD / 超时与输出上限），
    md5 与源文件一致（`271897eb…`，值不记录）。
    `.gitignore` 的 `.env` 只屏蔽名为 `.env` 的文件，不屏蔽它；已核对可入仓。
  - `scripts/apply-secrets.sh`：首次部署把 `secrets/deepseek.env` 落到
    `/etc/super-agent/.env`，0600；目标已存在时跳过、绝不覆盖；缺源文件时干净跳过；
    目标路径可用 `SUPER_AGENT_ENV_DEST` 覆盖（模拟验证用）；全程不打印密钥内容。
  - `secrets/README.md`：目录说明 + 密钥轮换办法（控制台吊销旧 key → 只改
    `DEEPSEEK_API_KEY` 一行入仓 → 每台部署机删旧 `.env` 重跑脚本并重启）。
  - `README.md` 私有构建节：写清拉取即部署五步（细粒度只读单库 token 克隆 →
    构建二进制 → apply-secrets.sh → seed-memory.sh → 启动守护进程）。
- 守护进程读密钥口径已核（`internal/llm/llm.go` ResolveAPIKey 与
  `internal/config/config.go` Load 同口径）：
  环境变量 → `$SUPER_AGENT_ENV` → `/etc/super-agent/.env` → `./.env`；
  `.env` 权限非 0600 时配置加载告警。

## 验证（全部通过）

- 干净克隆核对：`secrets/deepseek.env` md5 与本地源一致。
- 模拟部署（干净克隆 + `/tmp` 目标）：首次落盘 0600、md5 一致；二次运行跳过、不覆盖；
  预置被篡改的目标文件时同样跳过、内容不动；缺源文件时干净退出。
- API 核对：私密库 HEAD `7d50341`；公开库 `007-super-agent` 仍为 `8b54c6a`，未动。
- 全树扫描：除有意为之的 `secrets/deepseek.env` 外，无其他 `.env`/`.db`/`.key`/`.pem`；
  密钥名 `DEEPSEEK_API_KEY` 只出现在代码（llm.go/main.go/测试）、文档与密钥文件本身。
- GitHub token 未进仓库（90 天过期 2027-01-09，部署机上单独配置）；测试机过夜守护进程
  与 `/etc/super-agent/.env` 未动；临时克隆与 askpass 脚本已清理。

## 未做

- `deps.sh`/systemd 单元尚未入仓（仓库里本就没有），README 部署步骤里写的是
  “或按部署脚本；systemd 单元另配”——切流前补齐可再成一步。
- 密钥一旦泄露须按 `secrets/README.md` 轮换（旧 key 进 git 历史无法彻底抹除）。
