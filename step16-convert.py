#!/usr/bin/env python3
"""Step 16: convert #2 memory (L1 markdown + wiki) into 007 native format.

Input : /tmp/no2-memory-src  (tarball extracted from #2, read-only source)
Output: /tmp/no2-seed         (memories/no2-seed tree, ready to commit)
Rules : see step16-report.md section 2 (mapping).
"""
import hashlib
import os
import re
import shutil
import sys

SRC = "/tmp/no2-memory-src"
DST = "/tmp/no2-seed"

KINDS = ["entities", "concepts", "sources", "synthesis"]

stats = {"l1_lines": 0, "l1_skipped": 0, "wiki": {}, "wiki_summary_fallback": 0,
         "reference": 0, "root_copied": 0}


def md5_of(path):
    h = hashlib.md5()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def clean_md_inline(s):
    s = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", s)   # [text](url) -> text
    s = s.replace("**", "").replace("__", "").replace("`", "")
    s = re.sub(r"<[^>]+>", "", s)                    # html tags
    return s.strip()


def derive_summary(body_lines):
    """First plain paragraph line, else fallback."""
    in_meta = False
    for ln in body_lines:
        s = ln.strip()
        if not s:
            continue
        if s.startswith("```"):
            continue
        if s.startswith("## Metadata"):
            in_meta = True
            continue
        if in_meta:
            # Metadata block: "- key: value" lines; ends at next heading
            if s.startswith("#"):
                in_meta = False
            else:
                continue
        if s.startswith("#") or s.startswith(">") or s == "---":
            continue
        if re.match(r"^[-*+]\s", s) or re.match(r"^\d+[.)]\s", s):
            continue
        t = clean_md_inline(s)
        if len(t) < 4:
            continue
        r = list(t)
        return "".join(r[:120])
    stats["wiki_summary_fallback"] += 1
    return "二号 wiki 词条（原文见正文）"


def split_frontmatter(text):
    """Return (fm_dict, body_text). fm only if file starts with --- block."""
    if text.startswith("---\n") or text.startswith("---\r\n"):
        end = text.find("\n---", 4)
        if end != -1:
            fm_raw = text[4:end]
            fm = {}
            for line in fm_raw.splitlines():
                if ":" in line:
                    k, v = line.split(":", 1)
                    fm[k.strip()] = v.strip()
            return fm, text[end + 4:].lstrip("\n")
    return {}, text


def convert_wiki_file(src_path, kind_dir):
    stem = os.path.basename(src_path)[:-3]  # strip .md
    with open(src_path, encoding="utf-8", errors="replace") as f:
        text = f.read()
    fm, body = split_frontmatter(text)
    body_lines = body.splitlines()
    summary = derive_summary(body_lines)
    prov = (f"\n\n> 迁移来源：二号 wiki（2026-11-18 迁移）；原分类 {kind_dir}；"
            f"原路径 {fm.get('path', '—')}；原更新 {fm.get('updated', '—')}。\n")
    out = f"# {stem}\n\nSummary: {summary}\n\n" + body.rstrip() + prov
    return stem, out


def convert_l1():
    os.makedirs(os.path.join(DST, "l1"), exist_ok=True)
    sec_re = re.compile(r"^## \[(\d\d:\d\d)\]\s*(.*)$")
    for name in sorted(os.listdir(os.path.join(SRC, "l1"))):
        if not name.endswith(".md"):
            continue
        date = name[:-3]
        with open(os.path.join(SRC, "l1", name), encoding="utf-8",
                  errors="replace") as f:
            lines = f.read().splitlines()
        out_lines = []
        cur_time, cur_tag, cur_body = None, None, []
        def flush():
            if cur_time is None:
                return
            text = " ".join(x.strip() for x in cur_body)
            text = re.sub(r"\s+", " ", text).strip()
            text = text.removeprefix("- ").strip()
            if not text:
                stats["l1_skipped"] += 1
                return
            out_lines.append(
                f"{date}T{cur_time}:00+08:00\t[{cur_time}] {cur_tag} {text}")
            stats["l1_lines"] += 1
        for ln in lines:
            m = sec_re.match(ln)
            if m:
                flush()
                cur_time, cur_tag, cur_body = m.group(1), m.group(2).strip(), []
            elif cur_time is not None:
                cur_body.append(ln)
        flush()
        with open(os.path.join(DST, "l1", date + ".log"), "w",
                  encoding="utf-8") as f:
            f.write("\n".join(out_lines) + "\n")


def convert_wiki():
    for kind in KINDS + ["playbooks"]:
        src_dir = os.path.join(SRC, "wiki", kind)
        dst_dir = os.path.join(DST, "wiki", kind)
        n = 0
        for root, _dirs, files in os.walk(src_dir):
            for name in sorted(files):
                if not name.endswith(".md"):
                    continue
                src_path = os.path.join(root, name)
                rel_dir = os.path.relpath(root, src_dir)
                out_dir = dst_dir if rel_dir == "." else os.path.join(dst_dir, rel_dir)
                os.makedirs(out_dir, exist_ok=True)
                stem, out = convert_wiki_file(src_path, kind)
                with open(os.path.join(out_dir, stem + ".md"), "w",
                          encoding="utf-8") as f:
                    f.write(out)
                n += 1
        stats["wiki"][kind] = n
    # root files + _archive-index-pages: as-is
    wiki_dst = os.path.join(DST, "wiki")
    for name in ["index.md", "registry.md", "log.md", "lint-report.md"]:
        shutil.copy2(os.path.join(SRC, "wiki", name),
                      os.path.join(wiki_dst, name))
        stats["root_copied"] += 1
    shutil.copytree(os.path.join(SRC, "wiki", "_archive-index-pages"),
                    os.path.join(wiki_dst, "_archive-index-pages"))
    stats["root_copied"] += 3


def copy_reference():
    ref = os.path.join(DST, "reference")
    # agent/: .md only, keep structure
    for root, _dirs, files in os.walk(os.path.join(SRC, "agent")):
        for fn in files:
            if not fn.endswith(".md"):
                continue
            src = os.path.join(root, fn)
            rel = os.path.relpath(src, os.path.join(SRC, "agent"))
            dst = os.path.join(ref, "agent", rel)
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            shutil.copy2(src, dst)
            stats["reference"] += 1
    # protocols: as-is
    for root, _dirs, files in os.walk(os.path.join(SRC, "archive", "protocols")):
        for fn in files:
            src = os.path.join(root, fn)
            rel = os.path.relpath(src, os.path.join(SRC, "archive", "protocols"))
            dst = os.path.join(ref, "protocols", rel)
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            shutil.copy2(src, dst)
            stats["reference"] += 1
    # small refs: as-is
    for name in ["governance", "goals", "user", "reflections"]:
        s = os.path.join(SRC, name)
        if os.path.isdir(s):
            shutil.copytree(s, os.path.join(ref, name))
            for _r, _d, fs in os.walk(os.path.join(ref, name)):
                stats["reference"] += len(fs)
    shutil.copy2(os.path.join(SRC, "CONTINUITY_SNAPSHOT.md"),
                  os.path.join(ref, "CONTINUITY_SNAPSHOT.md"))
    stats["reference"] += 1


def write_docs(manifest_lines):
    readme = """# 二号种子记忆（memories/no2-seed）

来源：二号服务器 `/root/.pi/agent/memory/`（2026-11-18 只读拉取），l1_watcher.py（V6.1）写的 L1 日志与 Steel Will Wiki。
用途：私密构建 007（super-agent-007）的起点语料——部署时 `scripts/seed-memory.sh`
把 `l1/` 与 `wiki/` 拷进 `data/memory/`，让新二号带着二号的全部记忆上岗。
本目录绝不进公开库 007-super-agent。

布局：
- `l1/`：7 天 L1 日志，已转 007 原生格式（`RFC3339\\t文本`，每 `## [HH:MM]` 节一行）。
- `wiki/{entities,concepts,sources,synthesis}/`：336 个词条，已转 007 原生格式
  （`# 词条名` + `Summary:` 合成摘要 + 正文 + 迁移脚注）。
- `wiki/playbooks/`：5 个剧本，同格式；Recall 可召回，WriteWiki 不写此类。
- `wiki/index.md`、`registry.md`、`log.md`、`lint-report.md` 与
  `wiki/_archive-index-pages/`：原样。
- `l3/`：1180 个执行经验（lessons/decisions/patterns/cases 等），原样。
- `reference/protocols/`：108 个钢铁意志协议，原样。
- `reference/governance/`、`goals/`、`user/`、`reflections/`、
  `reference/CONTINUITY_SNAPSHOT.md`：小件参考，原样。

不迁的部分（见 step16-report.md 1.3–1.4）：sessions/（177M 原始会话）、
system/、candidates/、_archive/、wiki_candidates/、l3-*.tsv、watcher 状态文件；
二号服务器上没有 MEMORY.md，无物可迁。

时效性：种子 L1 按 007 的 7 天清理会自然过期，耐久知识在 wiki；
04:00 精炼每天提炼新 L1，记忆持续生长。种子拷入后 `scripts/seed-memory.sh`
会写备份标记，避免第 15 步备份把种子重复推一份进 `memories/<server_id>/`。
"""
    with open(os.path.join(DST, "README.md"), "w", encoding="utf-8") as f:
        f.write(readme)
    with open(os.path.join(DST, "MANIFEST.md"), "w", encoding="utf-8") as f:
        f.write("# 种子清单（2026-11-18 迁移）\n\n")
        f.write("来源：二号 `/root/.pi/agent/memory/`，只读拉取。\n\n")
        f.write("".join(manifest_lines))


def main():
    if os.path.exists(DST):
        shutil.rmtree(DST)
    convert_l1()
    convert_wiki()
    copy_reference()
    # manifest with md5s
    lines = []
    total_files, total_bytes = 0, 0
    for root, _d, files in os.walk(DST):
        for fn in sorted(files):
            p = os.path.join(root, fn)
            rel = os.path.relpath(p, DST)
            if rel in ("README.md", "MANIFEST.md"):
                continue
            lines.append(f"- `{rel}`  {os.path.getsize(p)}B  md5:{md5_of(p)}\n")
            total_files += 1
            total_bytes += os.path.getsize(p)
    header = (f"总数：{total_files} 个文件，{total_bytes} 字节。\n"
              f"L1 行数：{stats['l1_lines']}（跳过空节 {stats['l1_skipped']}）。\n"
              f"Wiki 各类：{stats['wiki']}；根文件 {_archive_note()}。\n"
              f"Reference 文件：{stats['reference']}。\n"
              f"Summary 合成兜底：{stats['wiki_summary_fallback']} 个。\n\n")
    write_docs([header] + lines)
    print(f"files={total_files} bytes={total_bytes}")
    print(f"l1_lines={stats['l1_lines']} skipped={stats['l1_skipped']}")
    print(f"wiki={stats['wiki']} root={stats['root_copied']} ref={stats['reference']} "
          f"summary_fallback={stats['wiki_summary_fallback']}")


def _archive_note():
    return f"{stats['root_copied']} 个（含 _archive-index-pages 3 个）"


if __name__ == "__main__":
    main()
