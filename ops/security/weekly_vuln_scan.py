#!/opt/007-venv/bin/python3
# -*- coding: utf-8 -*-
"""
007 每周漏洞巡检（只读，零 token，零模型调用）。
- Python 侧：只扫 /opt/007-venv（绝不扫系统 Python，否则 apt 旧包会一直误报）。
- uv scrapling 侧：用 venv 的 pip-audit 扫 uv 环境的包清单。
- Go 侧：govulncheck（调用图可达性，只报真正能打到的漏洞）。
- 豁免清单：ops/security/vuln_ignore.json（过期自动失效；reopen_when_fixed 且 PyPI 真有新版时自动解除）。
- 基线：/var/lib/super-agent/vuln_baseline.json（不在 git 里）。
- 分级通知：新增 + 核心网络包 -> 高（直接推）；新增中低 -> 摘要；无新增 -> 只记日志。
- 只读红线：本脚本绝不跑 pip install -U；修复永远走“人工确认 -> 冷备 venv -> 升级验证 -> 更新 lock”。
- 退出码：0 = 扫完且无新增高危；1 = 有新增发现（高危或中低）；2 = 巡检脚本异常（绝不能当成“安全”）。
"""
import json
import os
import subprocess
import sys
from datetime import date

VENV_PY = "/opt/007-venv/bin/python3"
VENV_PIP = "/opt/007-venv/bin/pip"
VENV_PIP_AUDIT = "/opt/007-venv/bin/pip-audit"
UV_PY = "/root/.local/share/uv/tools/scrapling/bin/python"
GOVULNCHECK = "/root/go/bin/govulncheck"
GO_DIR = "/root/super-agent-backup"
IGNORE_PATH = "/root/super-agent-007/ops/security/vuln_ignore.json"
BASELINE_PATH = "/var/lib/super-agent/vuln_baseline.json"
ALERT_PATH = "/var/lib/super-agent/vuln_alert.json"
LOG_PATH = "/var/log/super-agent-vuln-audit.log"

# 核心网络包：新漏洞出现即高危（pip-audit 不输出 CVSS 分数，所以用这张表当高危触发器）。
CORE_PACKAGES = {
    "brotli", "mcp", "yt-dlp", "httplib2", "urllib3", "requests",
    "httpx", "aiohttp", "cryptography", "playwright",
}

TIMEOUT = 300


def log(msg):
    line = "[%s] %s" % (_now(), msg)
    print(line, flush=True)
    try:
        with open(LOG_PATH, "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except Exception as e:
        print("[%s] 写日志失败: %s" % (_now(), e), flush=True)


def _now():
    import datetime
    return datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")


def run(cmd, cwd=None, input_text=None):
    """跑一条命令，返回 (returncode, stdout, stderr)。超时返回 (-99, '', 'timeout')。"""
    try:
        r = subprocess.run(
            cmd, cwd=cwd, input=input_text, capture_output=True,
            text=True, timeout=TIMEOUT,
        )
        return r.returncode, r.stdout, r.stderr
    except subprocess.TimeoutExpired:
        return -99, "", "timeout after %ss" % TIMEOUT
    except Exception as e:
        return -98, "", "exec error: %s" % e


def pip_audit_findings(argv, label):
    """跑 pip-audit，返回 (findings, error)。
    findings: [{ecosystem, package, version, vuln_id, fix_versions, description}]
    exit 0=干净，1=有发现，其他=异常。"""
    rc, out, err = run(argv)
    if rc == 0:
        return [], None
    if rc == 1:
        pass  # 有发现，继续解析
    elif rc in (-99, -98):
        return None, "%s 异常：%s" % (label, err)
    else:
        return None, "%s 异常：exit=%s stderr=%s" % (label, rc, err[:300])
    try:
        data = json.loads(out)
    except Exception as e:
        return None, "%s 输出解析失败：%s" % (label, e)
    findings = []
    for dep in data.get("dependencies", []):
        for v in dep.get("vulns", []):
            findings.append({
                "ecosystem": "python",
                "package": dep.get("name", "?"),
                "version": dep.get("version", "?"),
                "vuln_id": v.get("id", "?"),
                "fix_versions": v.get("fix_versions", []),
                "description": (v.get("description", "") or "")[:200],
            })
    return findings, None


def uv_scrapling_freeze():
    """取 uv scrapling 环境的包清单（pip freeze 文本）。"""
    for uv in ("/root/.local/bin/uv", "/usr/local/bin/uv", "/usr/bin/uv"):
        if os.path.exists(uv):
            rc, out, err = run([uv, "pip", "freeze", "--python", UV_PY])
            if rc == 0 and out.strip():
                return out
            return None
    return None


def govulncheck_findings():
    """跑 govulncheck -json，返回 (findings, error)。
    exit 0=干净，3=有发现，其他=异常。只取调用图可达的 finding。"""
    rc, out, err = run([GOVULNCHECK, "-json", "./..."], cwd=GO_DIR)
    if rc == 0:
        return [], None
    if rc == 3:
        pass
    elif rc in (-99, -98):
        return None, "govulncheck 异常：%s" % err
    else:
        return None, "govulncheck 异常：exit=%s stderr=%s" % (rc, err[:300])
    findings = []
    for line in out.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            obj = json.loads(line)
        except Exception:
            continue
        f = obj.get("finding")
        if not f:
            continue
        # 只保留调用图可达的（trace 里有调用链）；没有 trace 的当不可达，跳过。
        if not f.get("trace"):
            continue
        osv = f.get("osv", "") or ""
        findings.append({
            "ecosystem": "go",
            "package": f.get("package", "?"),
            "version": "?",
            "vuln_id": osv,
            "fix_versions": [f.get("fixedVersion")] if f.get("fixedVersion") else [],
            "description": (f.get("osv", "") or "")[:200],
        })
    return findings, None


def load_ignore():
    try:
        with open(IGNORE_PATH, encoding="utf-8") as f:
            return json.load(f).get("ignores", [])
    except Exception as e:
        log("豁免清单读取失败（当作无豁免）：%s" % e)
        return []


def fix_version_installable(package, fix_versions):
    """用 pip --dry-run 验证修复版是否真的能装进本机 venv。
    只查 PyPI 有没有这个版本号不够——还要过 yanked、Requires-Python 这两关
    （实测：soupsieve 3.0.2 在 PyPI 上有文件，但要求 Python >=3.11.5，本机 3.11.2 装不上）。
    装不上/查不到 -> False（保持豁免，安全默认）。--dry-run 不实际安装，只读。"""
    if not fix_versions:
        return False
    for ver in fix_versions:
        rc, out, err = run([VENV_PIP, "install", "--dry-run", "%s==%s" % (package, ver)])
        if rc == 0:
            return True
    return False


def is_ignored(f, ignores):
    """返回 (是否豁免, 原因)。过期或已修复 -> 不豁免。
    豁免清单里 ecosystem 写 python 的，同时覆盖 venv 和 uv scrapling 两个环境
    （同一包同一漏洞，不用写两遍）。"""
    today = date.today().isoformat()
    for ig in ignores:
        ig_eco = ig.get("ecosystem", "")
        f_eco = f["ecosystem"]
        eco_match = (ig_eco == f_eco) or (
            ig_eco == "python" and f_eco == "python-uv-scrapling")
        if not eco_match:
            continue
        if ig.get("package") != f["package"]:
            continue
        if ig.get("vuln_id") != f["vuln_id"]:
            continue
        exp = ig.get("expires_at", "")
        if exp and exp < today:
            return False, "豁免已过期 %s" % exp
        if ig.get("reopen_when_fixed") and fix_version_installable(f["package"], f["fix_versions"]):
            return False, "PyPI 已有可安装的修复版，豁免自动解除"
        return True, ig.get("reason", "")
    return False, ""


def load_baseline():
    try:
        with open(BASELINE_PATH, encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return {"last_scan_at": "", "reported": []}


def save_baseline(reported_keys):
    os.makedirs(os.path.dirname(BASELINE_PATH), exist_ok=True)
    with open(BASELINE_PATH, "w", encoding="utf-8") as f:
        json.dump(
            {"last_scan_at": _now(), "reported": sorted(reported_keys)},
            f, indent=2, ensure_ascii=False,
        )


def save_alert(payload):
    os.makedirs(os.path.dirname(ALERT_PATH), exist_ok=True)
    with open(ALERT_PATH, "w", encoding="utf-8") as f:
        json.dump(payload, f, indent=2, ensure_ascii=False)


def key_of(f):
    return "%s:%s:%s" % (f["ecosystem"], f["package"], f["vuln_id"])


def main():
    log("===== 每周漏洞巡检开始 =====")
    errors = []
    all_findings = []

    # 1) venv
    f1, e1 = pip_audit_findings(
        [VENV_PIP_AUDIT, "--format", "json", "--desc"], "venv pip-audit")
    if e1:
        errors.append(e1)
    else:
        all_findings.extend(f1)
        log("venv 扫完：%d 条漏洞记录" % len(f1))

    # 2) uv scrapling（用临时文件传包清单，stdin 在 subprocess 里不可靠）
    freeze = uv_scrapling_freeze()
    if freeze is None:
        log("uv scrapling 清单取不到，跳过该侧（不算异常）")
    else:
        import tempfile
        with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as tf:
            tf.write(freeze)
            tmp = tf.name
        try:
            f2, e2 = pip_audit_findings(
                [VENV_PIP_AUDIT, "-r", tmp, "--format", "json", "--desc"],
                "uv scrapling pip-audit")
        finally:
            try:
                os.unlink(tmp)
            except Exception:
                pass
        if e2:
            errors.append(e2)
        else:
            for x in f2:
                x["ecosystem"] = "python-uv-scrapling"
            all_findings.extend(f2)
            log("uv scrapling 扫完：%d 条漏洞记录" % len(f2))

    # 3) Go
    if os.path.exists(GOVULNCHECK):
        f3, e3 = govulncheck_findings()
        if e3:
            errors.append(e3)
        else:
            all_findings.extend(f3)
            log("Go 扫完：%d 条可达漏洞记录" % len(f3))
    else:
        log("govulncheck 未安装，跳过 Go 侧")

    # 4) 豁免过滤
    ignores = load_ignore()
    active, ignored = [], []
    for f in all_findings:
        ok, why = is_ignored(f, ignores)
        if ok:
            ignored.append((f, why))
        else:
            active.append((f, why))
    log("豁免过滤：%d 条被豁免，%d 条有效" % (len(ignored), len(active)))
    for f, why in ignored:
        log("  已豁免：%s（%s）" % (key_of(f), why))

    # 5) 与基线对比，找新增
    baseline = load_baseline()
    reported = set(baseline.get("reported", []))
    new_high, new_low = [], []
    for f, why in active:
        k = key_of(f)
        if k in reported:
            continue
        entry = dict(f)
        entry["note"] = why
        if f["package"] in CORE_PACKAGES:
            new_high.append(entry)
        else:
            new_low.append(entry)
    for f, _ in active:
        log("  有效：%s %s fix=%s" % (key_of(f), f["version"], ",".join(f["fix_versions"]) or "无"))

    # 6) 更新基线（有效项全记入，下次不再当“新增”；已修复的自动掉出）
    save_baseline({key_of(f) for f, _ in active})

    # 7) 写待通知文件 + 日志 + 退出码
    alert = {
        "scan_at": _now(),
        "scan_error": errors,
        "new_high": new_high,
        "new_low": new_low,
        "ignored_count": len(ignored),
        "active_count": len(active),
    }
    save_alert(alert)
    if errors:
        log("巡检脚本异常（绝不能当成安全）：%s" % "; ".join(errors))
        log("===== 巡检结束：异常 =====")
        return 2
    if new_high:
        log("新增高危 %d 条（需直接推）：%s" % (
            len(new_high), ", ".join(key_of(x) for x in new_high)))
    if new_low:
        log("新增中低 %d 条（进周报摘要）：%s" % (
            len(new_low), ", ".join(key_of(x) for x in new_low)))
    if not new_high and not new_low:
        log("无新增漏洞，静默记日志")
    log("===== 巡检结束 =====")
    return 1 if (new_high or new_low) else 0


if __name__ == "__main__":
    sys.exit(main())
