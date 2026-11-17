# cap-ops-pandoc

## 一句话
文档格式互转（md ↔ html/docx/latex），排版无关的纯结构转换器。

## 怎么调
pandoc in.md -o out.html # md → html pandoc in.md -o out.docx # md → Word（需要 docx 模板时加 --reference-doc） pandoc in.html -t markdown -o out.md # html → md（抓来的页面转纯文本常用） pandoc in.md -o out.pdf --pdf-engine=xelatex # 出 PDF 要 latex 引擎（本机未验证有） pandoc -s --toc in.md -o out.html # 独立文件 + 目录

## 坑
2.17 是老版本：`--embed-resources` 这个新名字可能不认（老名字 `--self-contained`）；fenced div 语法细节与 3.x 有差。；PDF 出不来不是 pandoc 的锅：要 `xelatex`/`wkhtmltopdf` 引擎，本机只验了 html，PDF 路径未验证 → 出 PDF 走 `pdf` 技能（`~/.pi/agent/git/…

## 实际效果
`pandoc --version` = `pandoc 2.17.1.1`。；实测：`pandoc p.md -o p.html`，输入 `# T` + 一个两列表格 → 输出 162 字节 HTML

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`pandoc --version` = `pandoc 2.17.1.1`。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/P-pandoc.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：2.17.1.1
