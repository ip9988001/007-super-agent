# cap-ops-tesseract

## 一句话
tesseract 5.3.0（apt 装）：命令行 OCR，把图片里的文字认成文本；配 `--oem 1 --psm 6` 认排版文本最稳。

## 怎么调
tesseract <图片> stdout -l chi_sim+eng --psm 6 # 认中英混合，输出到 stdout
tesseract <图片> out.txt -l eng # 写文件
tesseract <图片> stdout --psm 11 # 稀疏文字（散落的短词）逐块认

## 坑
没装中文语言包时 `-l chi_sim` 直接报错退出，先 `tesseract --list-langs` 看；扫描版 PDF 要先用 `pdftoppm` 或 magick 转 PNG 再认；手写体基本认不出，别当真。

## 实际效果
`tesseract --version` → `5.3.0` ✅（第 19 步 apt 实测）；`tesseract --list-langs` 列出已装语言。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`tesseract --version` → `5.3.0` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：第 19 步安装核验（--version 实测，2026-11-18）
