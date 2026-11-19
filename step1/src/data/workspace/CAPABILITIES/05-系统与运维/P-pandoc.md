# pandoc 2.17.1

> **一句话**：文档格式互转（md ↔ html/docx/latex），排版无关的纯结构转换器。

## 实际效果（实测）
- `pandoc --version` = `pandoc 2.17.1.1`。
- 实测：`pandoc p.md -o p.html`，输入 `# T` + 一个两列表格 → 输出 162 字节 HTML，
  `<h1 id="t">T</h1>` + `<table><thead>...` 结构正确，退出码 0。

## 怎么调
```
pandoc in.md -o out.html                    # md → html
pandoc in.md -o out.docx                    # md → Word（需要 docx 模板时加 --reference-doc）
pandoc in.html -t markdown -o out.md        # html → md（抓来的页面转纯文本常用）
pandoc in.md -o out.pdf --pdf-engine=xelatex # 出 PDF 要 latex 引擎（本机未验证有）
pandoc -s --toc in.md -o out.html           # 独立文件 + 目录
```

## 坑
- **2.17 是老版本**：`--embed-resources` 这个新名字可能不认（老名字 `--self-contained`）；fenced div 语法细节与 3.x 有差。
- **PDF 出不来不是 pandoc 的锅**：要 `xelatex`/`wkhtmltopdf` 引擎，本机只验了 html，PDF 路径未验证 → 出 PDF 走 `pdf` 技能（`~/.pi/agent/git/.../skills/pdf`）。
- docx 输出用默认模板会带 pandoc 的样式；要企业样式必须给 `--reference-doc`。
- 表格：pandoc 2.x 对超宽表格只做简单渲染，复杂合并单元格会丢 —— 要好看用 `docx`/`xlsx` 技能直写 OOXML。
