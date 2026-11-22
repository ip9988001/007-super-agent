# cap-ops-python

## 一句话
系统 Python（`/usr/bin/python3`），脚本层与 pip 层工具的底座，没有 pyenv/conda。

## 怎么调
python3 -V pip3 install --break-system-packages <pkg> # Debian 12 PEP668，必须带这个开关 python3 -m json.tool # 没 jq 时的兜底（本机已有 jq，见 J-jq.md） python3 script.py --help

## 坑
PEP668：直接 `pip3 install` 会被拒；`start.sh` 3 段装 semgrep 用的就是；pip 层随容器重建丢失（只有 apt 的 python3 本身在镜像里）。工具类包优先走 `uv tool`（见 `U-uv.md`）。

## 实际效果
`python3 -V` = `Python 3.11.2 (main, Oct 7 2026)`；`bandit 1.9.4` 的 venv 同样是 3.11.2。；常驻脚本都用它跑：`l1_watcher.py`（PID 95）、`refine_daily.py`（PID 96）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`python3 -V` = `Python 3.11.2 (main, Oct 7 2026)`；`bandit 1.9.4` 的 venv 同样是 3.11.2。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/P-python.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：3.11.2；常驻脚本都用它跑
