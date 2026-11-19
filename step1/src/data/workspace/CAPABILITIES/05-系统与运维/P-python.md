# python3 3.11.2

> **一句话**：系统 Python（`/usr/bin/python3`），脚本层与 pip 层工具的底座，没有 pyenv/conda。

## 实际效果（实测）
- `python3 -V` = `Python 3.11.2 (main, Oct 7 2026)`；`bandit 1.9.4` 的 venv 同样是 3.11.2。
- 常驻脚本都用它跑：`l1_watcher.py`（PID 95）、`refine_daily.py`（PID 96）。

## 怎么调
```
python3 -V
pip3 install --break-system-packages <pkg>   # Debian 12 PEP668，必须带这个开关
python3 -m json.tool                          # 没 jq 时的兜底（本机已有 jq，见 J-jq.md）
python3 script.py --help
```

## 坑
- **PEP668**：直接 `pip3 install` 会被拒；`start.sh` 3 段装 semgrep 用的就是
  `pip3 install --no-cache-dir --break-system-packages semgrep`。
- pip 层**随容器重建丢失**（只有 apt 的 python3 本身在镜像里）。工具类包优先走 `uv tool`（见 `U-uv.md`）。
- 没有 `python` 这个别名（只有 `python3`）；老脚本里写 `python x.py` 会 command not found。
- 系统 site-packages 被 `--break-system-packages` 装过的包，容器重建后需重装 —— 这是 `semgrep` 自愈段存在的原因。
