#!/bin/bash
# 007 第 19 步 · 第 1 批：基础工具链（测试机 VM124655，Debian 12）
# 目标：curl git jq python3（已有）+ node v22.23.4 npm 10.9.9 uv 0.13.5 yarn 1.22.22
#      pandoc 2.17.1.1 ffmpeg 5.1.9 rg 13.0.0 gh 2.103.2 java 25.0.5
# 来源：apt / Node.js 官方 tarball / GitHub release / 官方安装包，不走三号。
# 不碰 xray/nginx，不重启机器。
set -u
LOG=/tmp/sa007-batch1.log
exec > >(tee -a "$LOG") 2>&1
echo "=== batch1 start $(date -u +%FT%TZ) ==="
export DEBIAN_FRONTEND=noninteractive

echo "--- apt update ---"
apt-get update -qq

echo "--- apt: ffmpeg pandoc tesseract-ocr imagemagick mpv python3-pip ---"
apt-get install -y -qq ffmpeg pandoc tesseract-ocr imagemagick mpv python3-pip

echo "--- node v22.23.4 exact tarball ---"
cd /tmp
curl -fsSL -o node-v22.23.4.tar.xz https://nodejs.org/dist/v22.23.4/node-v22.23.4-linux-x64.tar.xz
tar -xJf node-v22.23.4.tar.xz -C /usr/local --strip-components=1
rm -f node-v22.23.4.tar.xz

echo "--- yarn 1.22.22 exact ---"
npm install -g yarn@1.22.22

echo "--- uv 0.13.5 exact (GitHub release) ---"
curl -fsSL -o uv-0.13.5.tar.gz https://github.com/astral-sh/uv/releases/download/0.13.5/uv-x86_64-unknown-linux-gnu.tar.gz
mkdir -p /tmp/uv-extract && tar -xzf uv-0.13.5.tar.gz -C /tmp/uv-extract
cp /tmp/uv-extract/uv /usr/local/bin/uv
cp /tmp/uv-extract/uvx /usr/local/bin/uvx
chmod +x /usr/local/bin/uv /usr/local/bin/uvx
rm -rf /tmp/uv-extract uv-0.13.5.tar.gz

echo "--- rg 13.0.0 exact (.deb) ---"
curl -fsSL -o ripgrep.deb https://github.com/BurntSushi/ripgrep/releases/download/13.0.0/ripgrep_13.0.0-1_amd64.deb
dpkg -i ripgrep.deb
rm -f ripgrep.deb

echo "--- gh 2.103.2 exact (.deb) ---"
curl -fsSL -o gh.deb https://github.com/cli/cli/releases/download/v2.103.2/gh_2.103.2_linux_amd64.deb
dpkg -i gh.deb
rm -f gh.deb

echo "--- java Temurin 25 (latest 25 GA, expect 25.0.5) ---"
mkdir -p /data/tools/jdk25
curl -fsSL -o jdk25.tar.gz "https://api.adoptium.net/v3/binary/latest/25/ga/linux/x64/jdk/hotspot/normal/eclipse"
tar -xzf jdk25.tar.gz -C /data/tools/jdk25 --strip-components=1
rm -f jdk25.tar.gz
ln -sfn /data/tools/jdk25/bin/java /usr/local/bin/java

echo "=== batch1 verify ==="
for c in curl git jq python3 node npm uv yarn pandoc ffmpeg rg gh java tesseract magick mpv; do
  if command -v "$c" >/dev/null 2>&1; then
    echo "$c YES $($c --version 2>/dev/null | head -1; $c -version 2>&1 | head -1 | grep -i version)"
  else
    echo "$c NO"
  fi
done
java -version 2>&1 | head -3
echo "=== batch1 done $(date -u +%FT%TZ) ==="
