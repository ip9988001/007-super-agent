#!/bin/bash
# deploy/deps.sh — 007 超级 Agent 系统依赖一键安装（公开版，无任何私密内容）
#
# 目标：一台全新 Debian 12 服务器（root 运行），装齐 007 运行所需的
# 系统依赖 + 语言运行时 + 向量模型，然后把完整工具链交给仓库根目录的
# install-toolchain.sh（第 19 步版本钉死版）。本脚本不重复钉版本，
# 工具链的具体版本以 install-toolchain.sh 为准，不许两处漂移。
#
# 本脚本装的（生产机 VM124655 实测版本）：
#   apt：git 2.39.5、curl 7.88.1、jq 1.6、python3 3.11.2、ffmpeg 5.1.9
#   Go 1.27.4（官方 tarball → /usr/local/go，软链 /usr/local/bin/go、gofmt）
#   Ollama 0.40.16（官方安装脚本）+ 模型 qwen3-embedding:0.6b
# install-toolchain.sh 装的（本脚本不重复）：Node v22.23.4、npm、uv、yarn、
# pandoc、rg、gh、Java Temurin 25.0.5、tesseract、magick、mpv、
# rea、codegraph、semgrep、ast-grep、trivy、gitleaks、bandit、pip-audit、
# ghidra、agent-reach、playwright、lightpanda、scrapling、camoufox、
# httpx、yt-dlp、edge-tts、hyperframes 等。
#
# 用法（在 007-super-agent 仓库根目录）：sudo bash deploy/deps.sh
# 可重复跑：已装且版本对得上的会跳过。
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "deps.sh: must run as root" >&2; exit 1; }
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export DEBIAN_FRONTEND=noninteractive

echo "=== deps.sh: apt base ==="
apt-get update -qq
apt-get install -y -qq git curl jq python3 python3-pip ffmpeg ca-certificates

echo "=== deps.sh: Go 1.27.4 ==="
if [ "$(go version 2>/dev/null | awk '{print $3}')" = "go1.27.4" ]; then
  echo "Go 1.27.4 already installed, skip"
else
  cd /tmp
  curl -fsSL -o go1.27.4.linux-amd64.tar.gz https://go.dev/dl/go1.27.4.linux-amd64.tar.gz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf go1.27.4.linux-amd64.tar.gz
  rm -f go1.27.4.linux-amd64.tar.gz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
fi
go version

echo "=== deps.sh: Ollama + embedding model ==="
if ! command -v ollama >/dev/null 2>&1; then
  curl -fsSL https://ollama.com/install.sh | sh
fi
if systemctl list-unit-files 2>/dev/null | grep -q '^ollama.service'; then
  systemctl enable --now ollama 2>/dev/null || ollama serve >/tmp/ollama.log 2>&1 &
else
  (ollama serve >/tmp/ollama.log 2>&1 &)
fi
for _ in $(seq 1 15); do ollama list >/dev/null 2>&1 && break; sleep 2; done
ollama pull qwen3-embedding:0.6b
ollama --version

echo "=== deps.sh: full toolchain via install-toolchain.sh ==="
if [ -x "$REPO_ROOT/install-toolchain.sh" ]; then
  bash "$REPO_ROOT/install-toolchain.sh"
else
  echo "deps.sh: $REPO_ROOT/install-toolchain.sh not found, toolchain step skipped" >&2
fi

echo "=== deps.sh: verify ==="
go version
python3 --version
node --version
java --version 2>&1 | head -1
git --version
jq --version
ffmpeg -version 2>&1 | head -1
ollama list
echo "deps.sh: DONE"
