# cap-design-design-write

## 一句话
`/design <品牌> <目标路径> --write` → 把 design-md 库的风格喂给 pi，让模型直接改目标源码并把 DESIGN.md 复制进项目根；备份完全靠 prompt 一句话，网关零兜底。源码 `bot-gateway.js:545-635`。

## 怎么调
cd <目标项目> git status --porcelain > /tmp/pre.txt # 1) 先确认工作区状态，未提交的改动先 stash 或 commit tar czf /tmp/snap-$(date +%s).tgz . -C .. 2>/dev/null || cp -a . /tmp/snap-$(date +%s) # 2) 真快照，别信 .bak # 3) 再发指令 /design stripe <目标路径> --write # 4) 回来核对 git status --porcelain; git diff --stat; diff -r /tmp/snap-xxx <目标项目> | head

## 坑
`.bak` 是模型自觉，不是机制：prompt 只写在"输出格式"第 6 条，模型完全可能先写后忘、或只对部分文件备份。真回滚靠 git 或外部快照。；覆盖 `DESIGN.md` 无提示：目标项目已有 DESIGN.md（比如 impeccable `document` 生成的）会被库里的外来品牌文件顶掉。

## 实际效果
分支判定：`DWRITE = dArgs.includes('--write')`，除了改 prompt 第 6 条文字，没有任何代码路径差异；只读：`6. 只读模式：禁止修改任何文件，只输出代码块。`

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /design；前置：写盘前先落盘备份。按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：分支判定：`DWRITE = dArgs.includes('--write')`，除了改 prompt 第 6 条文字，没有任何代码路径差异；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/04-设计与UI/G-design-write.md · 核实：2026-11-17（Step 1 文档已读（未独立实测运行时））
> 注意：写盘分支风险最高；备份靠 prompt，网关零兜底
