# 截图 / 视觉回归（UI 侧调用姿势）
## 一句话
设计体检之外想"用眼睛验"时，两条现成通道：`chrome-nosandbox --screenshot`（一次成型 0.8s）和 Playwright（多视口 / 全页 / 回归比对）；**浏览器栈本体不在这里讲**，实现细节全在 `01-取数与抓取/B-browser-stack.md`。

## 实际效果（实测 2026-10-26）
- `/data/bin/chrome-nosandbox --headless=new --hide-scrollbars --window-size=1280,800 --screenshot=/tmp/shot.png https://example.com` → 0.76s，75KB PNG（stderr 刷 DBus/NetworkManager 报错，**无害**）
- Playwright（`PLAYWRIGHT_BROWSERS_PATH=/data/cache/ms-playwright` + `/data/tools/uv/tools/playwright/bin/python`）→ 390x844 + `full_page=True`，0.77s，59KB
- 与体检联动：example.com 桌面 1280x800 detect 出 2 条（`straggler`），390x844 出 0 条 → **截图视口必须和 detect 视口对齐**，否则"看着没问题"和"报了问题"永远对不上

## 怎么调
```bash
# ① 最省事：一屏截图（不写脚本）
/data/bin/chrome-nosandbox --headless=new --hide-scrollbars \
  --window-size=1280,800 --screenshot=/tmp/shot.png "https://target"

# ② 多视口 / 全页（Playwright，必须指定内核路径）
export PLAYWRIGHT_BROWSERS_PATH=/data/cache/ms-playwright
/data/tools/uv/tools/playwright/bin/python - <<'PY'
from playwright.sync_api import sync_playwright
with sync_playwright() as p:
    b = p.chromium.launch(args=["--no-sandbox","--disable-dev-shm-usage"])
    for w,h,tag in [(390,844,"mobile"),(1280,800,"desktop")]:
        pg = b.new_page(viewport={"width":w,"height":h})
        pg.goto("https://target", timeout=30000)
        pg.screenshot(path=f"/tmp/{tag}.png", full_page=True)
PY
```
- 基线比对（同视口、同 full_page 才有意义）：`python3 -c "from PIL import Image,ImageChops as C;a=Image.open('/tmp/desktop.png').convert('RGB');b=Image.open('/tmp/desktop.new.png').convert('RGB');print(C.difference(a,b).getbbox())"`
- 回归流程：基线存 `/tmp/base/<page>-<viewport>.png` → 改样式 → 重拍 → `getbbox()` 非空即"有像素变化"（**不等于变坏**，仍要人眼过）
- 和 `/ui` 配合：先 `detect --json` 拿 `selector`，再同 viewport `pg.locator(sel).screenshot()` 精确定位

## 坑
- `chrome-nosandbox` 的 stderr 永远有 DBus/NetworkManager 噪音，**别当失败**；成功判据是 `… bytes written to file`。
- Playwright 不设 `PLAYWRIGHT_BROWSERS_PATH` → `executable doesn't exist`；系统 `python3` 里**没有** playwright，必须用上面那个 venv 解释器。
- 截图只证明"渲染成什么样"，**不产出任何规则判定**；条款级结论仍走 `detect`（见 `I-impeccable.md`）。
- 懒加载/滚动动画页面，`full_page=True` 会把未触发区域拍成空/半截 —— 先滚到顶，或分段截。
- 视觉回归对字体抗锯齿极敏感：同机同内核才可比，跨机器必满屏假 diff。
- `impeccable context` 报 `IMAGE_TOOLS: no image converter found` —— 转 WebP 之类它不做。

> 浏览器安装位置、三内核清单、CDP/交互式用法 → `01-取数与抓取/B-browser-stack.md`（不重复）。
