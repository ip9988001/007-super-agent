# cap-ops-magick

## 一句话
ImageMagick 6.9.11-60（apt 装）：图片格式转换/缩放/裁剪/拼图的标配；本机 `magick` 是指向 `/usr/bin/convert` 的软链。

## 怎么调
magick <in> <out> # 格式转换
magick <in> -resize 50% <out> # 等比缩放
magick <in> -crop 800x600+0+0 +repage <out> # 裁剪
identify <in> # 看尺寸/格式（`magick identify` 同义）

## 坑
Debian 12 没有 `/usr/bin/magick`，只有 `convert`：本机已建软链 `/usr/local/bin/magick → /usr/bin/convert`（与三号一致），别删；`magick` 与 `convert` 语法同源，但 7.x 文档的 `magick` 专属参数在 6.x 上没有。

## 实际效果
`magick --version` → `6.9.11-60` ✅（第 19 步 apt 实测）；软链 `/usr/local/bin/magick → /usr/bin/convert` 已验证。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`magick --version` → `6.9.11-60` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：第 19 步安装核验（--version 实测，2026-11-18）
