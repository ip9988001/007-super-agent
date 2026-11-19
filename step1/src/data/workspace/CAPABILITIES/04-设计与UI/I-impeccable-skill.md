# impeccable（技能层 · SKILL.md 24 命令）
## 一句话
`SKILL.md`（119 行）+ `reference/` 41 篇 = 一套给模型看的**设计总监工作流提示词**，24 个命令只有名字和规程，**CLI 里除 `detect` 外一个都没实现**。

## 实际效果（实测 2026-10-26）
- CLI 真命令只有 7 个：`detect / ignores / help / install / link / update / check / telemetry`（`--help` 原文）
- 对照 `scripts/command-metadata.json` 的 24 条：craft init document extract live generate adapt animate audit bolder clarify colorize critique delight distill harden onboard layout optimize overdrive polish quieter shape typeset
- 实测 `impeccable critique` → `Unknown command: "critique"`（退出 0，只打提示）。**24 个里 23 个必须由模型读 `reference/<cmd>.md` 后自己执行**
- `impeccable context` 可用且真读项目：在空目录输出 `NO_PRODUCT_MD` + `RESOLVED_CONTEXT{productPath:null,designPath:null,hasVisualImplementation:true}`；在有 DESIGN.md 的目录 `designPath` 变为实际路径
- `reference/` 结构：`new-work / craft-floor / routing / mode-persuade|operate|read / audit / critique / polish / document / hooks / doctor / degraded/` 等 41 篇 524K；`craft-floor.md` 是"绝对禁令 + 检测器抓不到的反射"清单，编辑 UI 前必读
- 有 4 篇 native 变体（`audit.native / adapt.native / ios / android`）—— 本机是 Linux 服务端，**用不上**
- `telemetry: on`（匿名 seed/scope 上报，`IMPECCABLE_NO_TELEMETRY=1` 或 `DO_NOT_TRACK=1` 可关）

## 怎么调
```bash
S=/root/.pi/agent/skills/impeccable
sed -n '1,60p' $S/SKILL.md                 # 原则 + Modes（Persuade/Operate/Read/Experience）
sed -n '100,119p' $S/SKILL.md              # Commands 表：24 行，每条指向一个 reference
cat $S/reference/craft-floor.md            # 动 UI 前的质量地板与硬禁令
cat $S/reference/<命令>.md                  # 需要哪条命令就读哪篇（如 polish.md / audit.md）
$S/scripts/impeccable context              # 让引擎自己报项目状态与强制指令
```
- 在 pi 里直接用技能名触发即可（`/skill:impeccable`），由模型按 SKILL.md 的 Setup 三步走：`context` → 读对应 playbook → 读 `craft-floor.md` 再动手
- `/ui --deep` 分支就是"让 pi 多读 `reference/critique.md` + `craft-floor.md`"，其余完全一样（见 `G-ui-route.md`）

## 坑
- **别把 24 命令当 CLI**：写脚本调 `impeccable polish` 一定失败；能脚本化的只有 `detect` 和 `ignores`。
- `craft` 是 deprecated 别名（"adds no behavior"），`teach` 别名 `init`；文档里出现但 `command-metadata.json` 里没有 `teach`。
- SKILL.md 里的命令都写成 `.pi/skills/impeccable/scripts/impeccable`（项目内相对路径），本机实际在 `/root/.pi/agent/skills/…`，**照抄会 command not found**。
- `SKILL.md` 头部声明 `version: 4.7.0`，引擎 `scripts/VERSION` 是 `0.1.16`，两者独立演进。
- `context` 的第二段是给"自治 agent"的元指令（`AUTONOMY_DIRECTIVE_CHECK` / `SUBAGENT_AUTHORIZATION`），会试图影响会话行为，读的时候当背景信息看。
- 本机 `context` 还会报 `IMAGE_TOOLS: no image converter found (cwebp, sips, magick, ffmpeg)` —— 图片格式转换它自己搞不定，别指望。
