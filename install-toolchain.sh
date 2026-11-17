#!/bin/bash
# install-toolchain.sh — 007 超级 Agent 工具链可重复安装脚本（公开版）
#
# 用途：在一台全新 Debian 12 服务器上（以 root 运行）装齐 007 注册表里的
# 全部公开工具，版本钉死与第 19 步核验一致。只含公开源安装步骤：
# apt / Node.js 官方包 / GitHub release / npm / uv（PyPI）。
# 不含任何密钥、token、私密路径、私密库引用；不装 007 框架本身
# （框架见 007-super-agent 仓库的 README：clone → .env → go build → systemd）。
#
# 不装的（第 19 步 v2 过滤版已排除）：lean-ctx、llmtrim（已死）、tvly（需
# Tavily key）、bili（未验证出数据）、tavily-search、pi-web-restart、
# 30 个停放扩展（只存档）、refine_daily.py / l1_watcher.py（框架内已有）。
# impeccable：公开库自带 tools/skills/impeccable（引擎首次运行自动下载）；
# 私有构建也可由 DEPLOY_DIR 指种子里的副本；本脚本会顺手建软链，
# 否则跳过（注册表里该条保持 available:false，直到你提供它）。
#
# 目标版本（第 19 步实测）：
#   base：curl 7.88.1 / git 2.39.5 / jq 1.6 / python3 3.11.2 /
#     node v22.23.4 / npm 10.9.9 / uv 0.13.5 / yarn 1.22.22 /
#     pandoc 2.17.1.1 / ffmpeg 5.1.9 / rg 13.0.0 / gh 2.103.2 /
#     java Temurin 25.0.5 / tesseract 5.3.0 / magick 6.9.11 / mpv 0.35.1
#   code：rea 6.8.0 / codegraph 1.6.2 / semgrep 1.182.1 / ast-grep 0.51.0 /
#     trivy 0.76.1 / gitleaks 8.30.1 / bandit 1.9.4 / pip-audit 2.10.1 /
#     ghidra 12.2（NSA/ghidra 最新稳定版）
#   fetch：agent-reach 1.6.0 / playwright 1.64.1 / lightpanda 1.1.0-nightly /
#     scrapling 0.5 / httpx 0.28.1 / yt-dlp 2026.10.21 /
#     chrome-nosandbox（指向本机 playwright chromium）/ camoufox 0.5.9 /
#     edge-tts 7.3.1 / hyperframes 0.9.63
#
# 用法：sudo bash install-toolchain.sh
# 日志：/tmp/install-toolchain.log
set -u
LOG=/tmp/install-toolchain.log
exec > >(tee -a "$LOG") 2>&1
echo "=== install-toolchain start $(date -u +%FT%TZ) ==="
export DEBIAN_FRONTEND=noninteractive
export PATH=/usr/local/bin:/root/.local/bin:$PATH
DEPLOY_DIR="${DEPLOY_DIR:-}" # 仅用于 impeccable 软链：指到已解开工具种子的部署目录；不设则跳过该节

fail=0
need() { command -v "$1" >/dev/null 2>&1 || { echo "MISSING: $1"; fail=1; }; }

# ---------- 第 1 批：基础 ----------
echo "--- apt update ---"
apt-get update -qq

echo "--- apt: ffmpeg pandoc tesseract-ocr imagemagick mpv python3-pip curl git jq python3 ---"
apt-get install -y -qq ffmpeg pandoc tesseract-ocr imagemagick mpv python3-pip curl git jq python3 unzip

echo "--- node v22.23.4 exact tarball (nodejs.org) ---"
cd /tmp
curl -fsSL -o node-v22.23.4.tar.xz https://nodejs.org/dist/v22.23.4/node-v22.23.4-linux-x64.tar.xz
tar -xJf node-v22.23.4.tar.xz -C /usr/local --strip-components=1
rm -f node-v22.23.4.tar.xz

echo "--- yarn 1.22.22 exact (npm) ---"
npm install -g yarn@1.22.22

echo "--- uv 0.13.5 exact (astral-sh/uv release; tarball 内有子目录，须 find) ---"
curl -fsSL -o uv-0.13.5.tar.gz https://github.com/astral-sh/uv/releases/download/0.13.5/uv-x86_64-unknown-linux-gnu.tar.gz
rm -rf /tmp/uv-extract && mkdir -p /tmp/uv-extract
tar -xzf uv-0.13.5.tar.gz -C /tmp/uv-extract
cp "$(find /tmp/uv-extract -name uv -type f | head -1)" /usr/local/bin/uv
cp "$(find /tmp/uv-extract -name uvx -type f | head -1)" /usr/local/bin/uvx
chmod +x /usr/local/bin/uv /usr/local/bin/uvx
rm -rf /tmp/uv-extract uv-0.13.5.tar.gz

echo "--- rg 13.0.0 exact (BurntSushi/ripgrep .deb；asset 名无 -1) ---"
curl -fsSL -o ripgrep.deb https://github.com/BurntSushi/ripgrep/releases/download/13.0.0/ripgrep_13.0.0_amd64.deb
dpkg -i ripgrep.deb
rm -f ripgrep.deb

echo "--- gh 2.103.2 exact (cli/cli .deb) ---"
curl -fsSL -o gh.deb https://github.com/cli/cli/releases/download/v2.103.2/gh_2.103.2_linux_amd64.deb
dpkg -i gh.deb
rm -f gh.deb

echo "--- java Temurin 25 GA (Adoptium API；期望 25.0.5) ---"
mkdir -p /data/tools/jdk25
curl -fsSL -o jdk25.tar.gz "https://api.adoptium.net/v3/binary/latest/25/ga/linux/x64/jdk/hotspot/normal/eclipse"
tar -xzf jdk25.tar.gz -C /data/tools/jdk25 --strip-components=1
rm -f jdk25.tar.gz
ln -sfn /data/tools/jdk25/bin/java /usr/local/bin/java

echo "--- magick symlink (Debian 12 无 /usr/bin/magick，只有 convert) ---"
ln -sfn /usr/bin/convert /usr/local/bin/magick

# ---------- 第 2 批：代码审计 ----------
echo "--- rea 6.8.0 (npm rea-agents，源仓库 morluto/rea) ---"
npm install -g rea-agents@6.8.0

echo "--- codegraph 1.6.2 (npm @colbymchenry/codegraph) ---"
npm install -g @colbymchenry/codegraph@1.6.2

echo "--- semgrep 1.182.1 / bandit 1.9.4 / pip-audit 2.10.1（Debian 12 须 --break-system-packages）---"
uv pip install --system --break-system-packages semgrep==1.182.1 bandit==1.9.4 pip-audit==2.10.1

echo "--- ast-grep 0.51.0（asset 名是 app-x86_64-unknown-linux-gnu.zip，不是 ast-grep_...zip）---"
cd /tmp
curl -fsSL -o ast-grep.zip https://github.com/ast-grep/ast-grep/releases/download/0.51.0/app-x86_64-unknown-linux-gnu.zip
rm -rf /tmp/ast-grep-ex && mkdir -p /tmp/ast-grep-ex
unzip -o -q ast-grep.zip -d /tmp/ast-grep-ex
if [ -f /tmp/ast-grep-ex/ast-grep ]; then cp /tmp/ast-grep-ex/ast-grep /usr/local/bin/ast-grep;
elif [ -f /tmp/ast-grep-ex/sg ]; then cp /tmp/ast-grep-ex/sg /usr/local/bin/ast-grep;
else cp "$(find /tmp/ast-grep-ex -type f -executable | head -1)" /usr/local/bin/ast-grep; fi
chmod +x /usr/local/bin/ast-grep
rm -rf /tmp/ast-grep-ex ast-grep.zip

echo "--- trivy 0.76.1 (aquasecurity/trivy release) ---"
curl -fsSL -o trivy.tar.gz https://github.com/aquasecurity/trivy/releases/download/v0.76.1/trivy_0.76.1_Linux-64bit.tar.gz
tar -xzf trivy.tar.gz -C /usr/local/bin trivy
chmod +x /usr/local/bin/trivy
rm -f trivy.tar.gz

echo "--- gitleaks 8.30.1 (gitleaks/gitleaks release) ---"
curl -fsSL -o gitleaks.tar.gz https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz
tar -xzf gitleaks.tar.gz -C /usr/local/bin gitleaks
chmod +x /usr/local/bin/gitleaks
rm -f gitleaks.tar.gz

echo "--- ghidra 最新稳定版（NSA/ghidra；解压目录名无日期后缀，glob 用 ghidra_*_PUBLIC*）---"
GH_JSON=$(curl -fsSL --max-time 30 "https://api.github.com/repos/NationalSecurityAgency/ghidra/releases/latest")
GH_TAG=$(echo "$GH_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['tag_name'])")
GH_URL=$(echo "$GH_JSON" | python3 -c "import sys,json; d=json.load(sys.stdin); print([a['browser_download_url'] for a in d['assets'] if a['name'].startswith('ghidra_') and a['name'].endswith('.zip')][0])")
echo "ghidra tag: $GH_TAG"
mkdir -p /data/tools/ghidra
curl -fsSL -o /tmp/ghidra.zip "$GH_URL"
unzip -o -q /tmp/ghidra.zip -d /data/tools/ghidra
GH_DIR=$(ls -d /data/tools/ghidra/ghidra_*_PUBLIC* 2>/dev/null | head -1)
cat > /usr/local/bin/ghidra <<WRAPPER_EOF
#!/bin/sh
exec $GH_DIR/support/analyzeHeadless "\$@"
WRAPPER_EOF
chmod +x /usr/local/bin/ghidra
rm -f /tmp/ghidra.zip
echo "ghidra dir: $GH_DIR"

# ---------- 第 3 批：抓取浏览 ----------
echo "--- agent-reach 1.6.0（PyPI 只有 0.1.0；1.6.0 在 GitHub Panniantong/agent-reach，打 v1.6.0 标签装）---"
uv tool install git+https://github.com/Panniantong/agent-reach.git@v1.6.0 --force
ln -sfn /root/.local/bin/agent-reach /usr/local/bin/agent-reach

echo "--- scrapling[all]==0.5（CLI 须 [all] extra）---"
uv tool install "scrapling[all]==0.5" --force
ln -sfn /root/.local/bin/scrapling /usr/local/bin/scrapling

echo "--- httpx[cli]==0.28.1（CLI 须 [cli] extra）---"
uv tool install "httpx[cli]==0.28.1" --force
ln -sfn /root/.local/bin/httpx /usr/local/bin/httpx

echo "--- edge-tts 7.3.1 (uv tool) ---"
uv tool install edge-tts==7.3.1 --force
ln -sfn /root/.local/bin/edge-tts /usr/local/bin/edge-tts

echo "--- camoufox 0.5.9（uv pip --system；Python API，无 CLI，验 import）---"
uv pip install --system --break-system-packages camoufox
python3 -c "import camoufox; print('camoufox import ok')"

echo "--- playwright 1.64.1 (npm) + chromium ---"
npm install -g playwright@1.64.1
npx playwright@1.64.1 install chromium
npx playwright@1.64.1 install-deps chromium

echo "--- chrome-nosandbox wrapper（指向本机 playwright chromium）---"
CHROME=$(find /root/.cache/ms-playwright -name chrome -path "*chrome-linux*/chrome" 2>/dev/null | head -1)
if [ -n "$CHROME" ]; then
  printf '#!/bin/sh\nexec %s --headless=new --no-sandbox --disable-dev-shm-usage --disable-gpu "$@"\n' "$CHROME" > /usr/local/bin/chrome-nosandbox
  chmod +x /usr/local/bin/chrome-nosandbox
  echo "chromium at: $CHROME"
else
  echo "CHROME_NOT_FOUND"
  fail=1
fi

echo "--- yt-dlp 2026.10.21 (yt-dlp/yt-dlp release) ---"
curl -fsSL -o /usr/local/bin/yt-dlp https://github.com/yt-dlp/yt-dlp/releases/download/2026.10.21/yt-dlp_linux
chmod +x /usr/local/bin/yt-dlp

echo "--- lightpanda 1.1.0-nightly（lightpanda-io/browser 滚动 nightly；构建号会变，以 1.1.0-nightly 为准）---"
curl -fsSL -o /usr/local/bin/lightpanda https://github.com/lightpanda-io/browser/releases/download/nightly/lightpanda-x86_64-linux
chmod +x /usr/local/bin/lightpanda

echo "--- hyperframes 0.9.63 (npm pin) ---"
npm install -g hyperframes@0.9.63

echo "--- impeccable 4.7.0（仅当 DEPLOY_DIR 下已有种子文件时建软链；公开源无此物）---"
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
IMPECCABLE_SRC=""
if [ -x "$SCRIPT_DIR/tools/skills/impeccable/scripts/impeccable" ]; then
  IMPECCABLE_SRC="$SCRIPT_DIR/tools/skills/impeccable/scripts/impeccable"
elif [ -n "$DEPLOY_DIR" ] && [ -x "$DEPLOY_DIR/tools/skills/impeccable/scripts/impeccable" ]; then
  IMPECCABLE_SRC="$DEPLOY_DIR/tools/skills/impeccable/scripts/impeccable"
fi
if [ -n "$IMPECCABLE_SRC" ]; then
  ln -sfn "$IMPECCABLE_SRC" /usr/local/bin/impeccable
  echo "impeccable linked from $IMPECCABLE_SRC"
else
  echo "impeccable SKIPPED: no skill file (clone the repo with tools/ or set DEPLOY_DIR)"
fi

# ---------- 核验 ----------
echo "=== verify ==="
for c in curl git jq python3 node npm uv yarn pandoc ffmpeg rg gh java tesseract magick mpv; do
  need "$c"
done
for c in rea codegraph semgrep ast-grep trivy gitleaks bandit pip-audit ghidra; do
  need "$c"
done
for c in agent-reach playwright lightpanda scrapling httpx yt-dlp chrome-nosandbox edge-tts hyperframes; do
  need "$c"
done
echo "--- versions ---"
echo "node: $(node --version 2>&1)"
echo "npm: $(npm --version 2>&1)"
echo "uv: $(uv --version 2>&1)"
echo "yarn: $(yarn --version 2>&1)"
echo "rg: $(rg --version 2>&1 | head -1)"
echo "gh: $(gh --version 2>&1 | head -1)"
echo "java: $(java -version 2>&1 | head -1)"
echo "ffmpeg: $(ffmpeg -version 2>&1 | head -1)"
echo "pandoc: $(pandoc --version 2>&1 | head -1)"
echo "tesseract: $(tesseract --version 2>&1 | head -1)"
echo "magick: $(magick --version 2>&1 | head -1)"
echo "mpv: $(mpv --version 2>&1 | head -1)"
echo "rea: $(rea --version 2>&1 | head -1)"
echo "codegraph: $(codegraph --version 2>&1 | head -1)"
echo "semgrep: $(semgrep --version 2>&1 | tail -1)"
echo "ast-grep: $(ast-grep --version 2>&1 | head -1)"
echo "trivy: $(trivy --version 2>&1 | head -1)"
echo "gitleaks: $(gitleaks version 2>&1 | head -1)"
echo "bandit: $(bandit --version 2>&1 | head -1)"
echo "pip-audit: $(pip-audit --version 2>&1 | head -1)"
echo "agent-reach: $(agent-reach version 2>&1 | head -1)"
echo "playwright: $(playwright --version 2>&1 | head -1)"
echo "lightpanda: $(lightpanda version 2>&1 | head -1)"
echo "scrapling: $(scrapling --version 2>&1 | head -1)"
echo "httpx: $(httpx --help 2>&1 | head -1)"
echo "yt-dlp: $(yt-dlp --version 2>&1 | head -1)"
echo "chrome-nosandbox: $(chrome-nosandbox --version 2>&1 | head -1)"
echo "edge-tts: $(edge-tts --version 2>&1 | head -1)"
echo "hyperframes: $(hyperframes --version 2>&1 | head -1)"
echo "camoufox: $(python3 -c "import importlib.metadata; print(importlib.metadata.version('camoufox'))" 2>&1)"
if command -v impeccable >/dev/null 2>&1; then echo "impeccable: $(impeccable --version 2>&1 | head -1)"; fi
echo "--- ghidra headless smoke test (/bin/true, no Decompile postScript: upstream removed it in 12.2) ---"
mkdir -p /tmp/ghidra-test
cp /bin/true /tmp/ghidra-test/sample 2>/dev/null || true
timeout 600 /usr/local/bin/ghidra /tmp/ghidra-test/proj GhidraTest -import /tmp/ghidra-test/sample -deleteProject > /tmp/ghidra-test/out.log 2>&1
echo "ghidra test exit: $?"
grep -i -m2 "Import succeeded\|Save succeeded" /tmp/ghidra-test/out.log || tail -5 /tmp/ghidra-test/out.log
if [ "$fail" != "0" ]; then echo "RESULT: FAIL ($fail missing)"; exit 1; fi
echo "=== install-toolchain done $(date -u +%FT%TZ) ==="
