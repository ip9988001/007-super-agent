#!/bin/bash
# sa007-redeploy-step2022q.sh — 部署备份 Runner 读 .env 兜底版（step2022q）到测试机
# 用法：bash sa007-redeploy-step2022q.sh
# 背景：2026-11-19 用户要求备份 Runner 自己读 .env 兜底，不再依赖启动时导出 .env。
# 改动：internal/backup/backup.go（resolveToken：环境变量 → .env 文件，照抄 llm
# 的 ResolveAPIKey 口径；sanitize 也走 resolveToken）、cmd/super-agent/main.go
# （接线 EnvFile；新增 backup-run 手动触发子命令）、回归测试两个。
# 追加修复（同一步重建）：pull --rebase 也带 -c 身份——rebase 要 committer 身份，
# 首版二进制实测：备份提交 b2399bf 已写好，pull 在这一步报
# "unable to auto-detect email address"，仓库留在 rebase 中途；已加回归测试
# TestPullCarriesIdentityFlags。本脚本部署的是含该修复的重建二进制。
# 注意：step2022p 脚本里“启动前必须导出 .env”的要求已作废——Runner 自己读
# /etc/super-agent/.env，本脚本故意不导出就启动（plain nohup），并核对守护进程
# 环境里没有 GITHUB_TOKEN，以此证明兜底生效。导出不再是必需的。
# 步骤：传新二进制到二号 → 二号转测试机 → 停旧守护进程(step2022p)
# → 换二进制 → 不导出直接启动 → 校验（备份开启行、注册表 84、6262、环境无 Token）。
set -e
BIN=/tmp/super-agent.step2022q
[ -f "$BIN" ] || { echo "missing binary $BIN"; exit 1; }
md5sum "$BIN"
# 1) 传到二号中转
scp -i /home/hatch/.ssh/id_ed25519 -P 6260 -o BatchMode=yes -o ConnectTimeout=20 -o StrictHostKeyChecking=accept-new -o ProxyCommand="nc -X connect -x hatch-egress-proxy:3130 %h %p" "$BIN" root@100.117.8.35:/tmp/sa007-step2022q-super-agent
echo "scp-to-no2 OK"
# 2) 二号转测试机（在二号上跑 scp；scp 用 < /dev/null，密码走 SSHPASS）
PASS=$(awk '/45.140.88.10/{print $NF}' ~/.config/server-credentials/dogyun)
printf '%s\n' "$PASS" | ~/workspace/pi-mail/ssh_pi.sh 'read P; export SSHPASS="$P"; sshpass -e scp -P 9701 -o StrictHostKeyChecking=accept-new /tmp/sa007-step2022q-super-agent root@45.140.88.10:/root/super-agent-007/super-agent.new < /dev/null && echo "scp-to-test OK"'
# 3) 测试机上部署（base64 管道进 ssh，内层 ssh 后不加 < /dev/null）
cat > /tmp/sa007-deploy-step2022q-run.sh << 'RUN'
set -e
cd /root/super-agent-007
md5sum super-agent.new
grep -c '^BACKUP_' /etc/super-agent/.env
# 停旧守护进程
OLDPID=$(pgrep -x super-agent || true)
echo "old daemon pid: $OLDPID"
[ -n "$OLDPID" ] && kill -TERM $OLDPID || true
for i in $(seq 1 15); do pgrep -x super-agent >/dev/null || break; sleep 1; done
pgrep -x super-agent && { echo "OLD DAEMON STILL ALIVE"; exit 1; } || echo "old daemon stopped"
# 换二进制并启动（故意不导出 .env：Runner 自己读 .env 兜底）
cp super-agent super-agent.bak-step2022q
mv super-agent.new super-agent
chmod +x super-agent
md5sum super-agent
nohup ./super-agent > /tmp/sa007-deploy-step2022q-20261119.log 2>&1 &
sleep 6
echo "new daemon pid: $(pgrep -x super-agent)"
grep -E "memory backup (enabled|disabled)" /tmp/sa007-deploy-step2022q-20261119.log
grep -E "registry ready|gateway ready|self-evolve loop started" /tmp/sa007-deploy-step2022q-20261119.log
(ss -ltnp 2>/dev/null || netstat -ltnp 2>/dev/null) | grep 6262 || echo "6262 ss check inconclusive"
# 负对照：守护进程环境里必须没有 GITHUB_TOKEN（证明没导出也能跑）
echo -n "daemon env GITHUB_TOKEN lines: "
tr '\0' '\n' < /proc/$(pgrep -x super-agent)/environ | grep -c '^GITHUB_TOKEN=' || true
echo -n "daemon env BACKUP_ENABLED lines: "
tr '\0' '\n' < /proc/$(pgrep -x super-agent)/environ | grep -c '^BACKUP_ENABLED=' || true
RUN
B64=$(base64 -w0 /tmp/sa007-deploy-step2022q-run.sh)
PASS=$(awk '/45.140.88.10/{print $NF}' ~/.config/server-credentials/dogyun)
printf '%s\n' "$PASS" | ~/workspace/pi-mail/ssh_pi.sh "read P; export SSHPASS=\"\$P\"; echo $B64 | sshpass -e ssh -p 9701 -o StrictHostKeyChecking=accept-new root@45.140.88.10 'base64 -d | bash -s'"
echo "DEPLOY DONE"
