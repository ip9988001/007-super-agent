echo "=== runtimes ==="
for c in go python3 node npm pip3 cargo rustc java ruby php deno bun; do
  if command -v $c >/dev/null 2>&1; then echo "$c: $($c --version 2>&1 | head -1)"; else echo "$c: 未安装"; fi
done
echo "=== network isolation tools ==="
for c in unshare nsenter iptables nft bwrap firejail; do
  if command -v $c >/dev/null 2>&1; then echo "$c: 已安装"; else echo "$c: 未安装"; fi
done
echo "=== unshare net test ==="
unshare -n sh -c 'echo 内网隔离OK; ping -c1 -W2 8.8.8.8 >/dev/null 2>&1 && echo 外网仍通 || echo 外网已断' 2>&1 | head -3
echo "=== disk/mem ==="
df -h / | tail -1
free -h | head -2
echo "=== github token on server ==="
ls -la /etc/super-agent/.env 2>/dev/null && grep -c GITHUB /etc/super-agent/.env 2>/dev/null || echo "无 /etc/super-agent/.env 或无 GITHUB 项"
echo "=== curl to github api (anon rate limit) ==="
curl -s https://api.github.com/rate_limit | head -20
