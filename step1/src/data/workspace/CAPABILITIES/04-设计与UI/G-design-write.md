# `/design --write`（写盘分支 · 风险最高）
## 一句话
`/design <品牌> <目标路径> --write` → 把 design-md 库的风格喂给 pi，**让模型直接改目标源码并把 DESIGN.md 复制进项目根**；备份完全靠 prompt 一句话，网关零兜底。源码 `bot-gateway.js:545-635`。

## 实际效果（只读代码判断 + 手工核对，未对真实项目执行）
- 分支判定：`DWRITE = dArgs.includes('--write')`，除了改 prompt 第 6 条文字，**没有任何代码路径差异**
  - 只读：`6. 只读模式：禁止修改任何文件，只输出代码块。`
  - 写盘：`6. 写盘模式：允许改目标源码，每个被改文件先复制 .bak 再写；并把 DESIGN.md 复制到目标项目根。`
- 执行者：`runPi(prompt, deep?420000:200000)` → `pi --mode json -p <prompt>`（cwd=`/data/workspace`），带**全部** read/bash/edit/write 工具，无沙箱、无只读开关 → 模型说改就改
- 无任何网关侧保障：不 `git status`、不 `tar` 快照、不比 diff、不改完回滚、不校验 `.bak` 是否真生成、不限制可写目录
- 同类三条 `--write` 的备份口径还不一致：
  - `/ui --write`：`先复制 .bak 再写`（无命名规则）
  - `/design --write`：同上，另加"复制 DESIGN.md 进项目根"
  - `/seo --write`：`<文件>.bak-$(date +%Y%m%d-%H%M)`（唯一带时间戳的）
- `DESIGN.md` 复制进根目录 = **覆盖风险**：目标已有 DESIGN.md 时无备份、无提示（reference/document.md 生成的 DESIGN.md 会被无害覆盖）
- 写盘模式与只读模式**超时相同**（200s / 420s）：文件多时会在写到一半时被 SIGKILL → 半改状态 + 部分文件有 `.bak` 部分没有

## 怎么调（用前必做的手工兜底）
```bash
cd <目标项目>
git status --porcelain > /tmp/pre.txt          # 1) 先确认工作区状态，未提交的改动先 stash 或 commit
tar czf /tmp/snap-$(date +%s).tgz . -C .. 2>/dev/null || cp -a . /tmp/snap-$(date +%s)   # 2) 真快照，别信 .bak
# 3) 再发指令
/design stripe <目标路径> --write
# 4) 回来核对
git status --porcelain; git diff --stat; diff -r /tmp/snap-xxx <目标项目> | head
```
- 要"方案不落盘"就**不要加 `--write`**（默认只读，出代码块给人贴）
- 只想要 tokens：`/design stripe`（不给目标路径）→ 只出设计语言，零写盘风险
- 大项目分步：先 `/design <brand> <path>`（只读拿方案）→ 人工确认 → 再逐文件手工落盘（路径按空白 join，带空格会被截断）

## 坑
- **`.bak` 是模型自觉，不是机制**：prompt 只写在"输出格式"第 6 条，模型完全可能先写后忘、或只对部分文件备份。真回滚靠 git 或外部快照。
- **覆盖 `DESIGN.md` 无提示**：目标项目已有 DESIGN.md（比如 impeccable `document` 生成的）会被库里的外来品牌文件顶掉。
- **外来 tokens 会污染后续体检**：把库里某品牌的 DESIGN.md 复制进项目根后，`impeccable detect` 会用它做 `design-system-color/font/radius` 比对（实测：本地 DESIGN.md 里无 `#ff0000` → 出 `design-system-color` advisory），对外来品牌照抄等于给自己埋一堆假 drift。
- `--deep` 在 `/design` 里**没写进帮助文本**（代码却认），只影响超时 200→420s。
- 品牌名走"精确 → 子串"两级匹配：`line` 会命中 `linear.app`，写错品牌不报错、静默套错风格。
- 只读模式下如果模型违规改文件，网关**看不出来**（不检查 git status），回复里也不会有任何痕迹。

> 品牌库本体与 tokens 真值 → `03-SEO与内容/D-design-md.md`；DESIGN.md 自产流程边界 → `D-design-md-自产.md`。
