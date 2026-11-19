#!/usr/bin/env python3
"""用 Camoufox（反检测 Firefox）抓一个 URL 的渲染后 DOM。

用法：
  python3 camoufox-fetch.py --url https://example.com --out /abs/path/page.html [--wait 15] [--text /abs/path/page.txt]

说明：
- Camoufox 自带指纹伪装，能过 Cloudflare Turnstile 这类机器人验证；
  普通 curl / 无头 Chrome 拿到的常是 40 字节空壳或验证页。
- 依赖：系统 python3 能 import camoufox；系统装了 libgtk-3-0 等
  Firefox 依赖（Debian: apt install libgtk-3-0 ...）；首次运行会
  自动从 GitHub 下载约 1.3GB 的 Camoufox 浏览器到 ~/.cache/camoufox/。
- --wait 是加载后等待 JS 填充的秒数（默认 15）；--text 可选，
  另存一份纯文本正文（去标签）。
- 成功时打印一行摘要并 exit 0；失败打印 ERROR 并 exit 1。
"""
import argparse
import re
import sys


def main() -> int:
    ap = argparse.ArgumentParser(description="Camoufox 渲染抓取")
    ap.add_argument("--url", required=True, help="要抓的 URL")
    ap.add_argument("--out", required=True, help="DOM 落盘绝对路径（.html）")
    ap.add_argument("--wait", type=int, default=15, help="加载后等待秒数，默认 15")
    ap.add_argument("--text", default="", help="可选：纯文本正文落盘路径（.txt）")
    args = ap.parse_args()

    try:
        from camoufox.sync_api import Camoufox
    except ImportError as e:
        print(f"ERROR: import camoufox 失败：{e}", file=sys.stderr)
        return 1

    try:
        with Camoufox(headless=True) as browser:
            page = browser.new_page()
            page.goto(args.url, timeout=60000)
            page.wait_for_timeout(args.wait * 1000)
            dom = page.content()
    except Exception as e:
        print(f"ERROR: {type(e).__name__}: {e}", file=sys.stderr)
        return 1

    with open(args.out, "w", encoding="utf-8") as f:
        f.write(dom)
    summary = f"dom_bytes={len(dom)} turnstile={'turnstile' in dom.lower() or 'cf-turnstile' in dom}"
    if args.text:
        text = re.sub(r"<script[\s\S]*?</script>", " ", dom, flags=re.I)
        text = re.sub(r"<style[\s\S]*?</style>", " ", text, flags=re.I)
        text = re.sub(r"<[^>]+>", " ", text)
        text = re.sub(r"\s+", " ", text).strip()
        with open(args.text, "w", encoding="utf-8") as f:
            f.write(text)
        summary += f" text_chars={len(text)}"
    print(f"OK {args.url} -> {args.out} {summary}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
