# ❌ playbooks（剧本库）— 不存在

> **一句话**：AGENTS.md 里写的 `memory/wiki/playbooks/<任务族>/` **在本机不存在**，剧本库目前是**纸面约定，零实体**。

## 实际效果（实测）
- `find /root/.pi/agent/memory -type d | grep -i play` → **NONE**。
- 全盘 `find / -type d -name "playbooks"` → **只有一处**：
  `/data/workspace/reverse-skill-quarantine/skills/pentest-tools/src-hunter/references/playbooks`
  —— 那是被隔离的第三方技能自带的**渗透测试剧本**，与钢铁意志的「任务族剧本库」无关。
- `memory/wiki/` 下只有 `concepts/` `entities/` `sources/` `synthesis/` 四个分类目录，**没有 playbooks**。

## 怎么调
现在无可调。**要建的话**（本次不改，仅记为方案）：
```
mkdir -p /root/.pi/agent/memory/wiki/playbooks/<任务族>/     # 如 抓取/ 审计/ 交付/
# 每条剧本建议字段：outcome(ok|fail) · 触发指纹 · 步骤 · 武器 · 耗时 · 坑
```
读侧应走 `recall_memory`，命中后再打开具体剧本文件。

## 坑
- **AGENTS.md 与实盘不一致**：宪法里写「剧本库：`memory/wiki/playbooks/<任务族>/`（成功/失败同库，用 `outcome` 字段区分）」，
  实盘无此目录 → 任何"照剧本执行"的指令都会落到空处，只能退回「查工具索引 → 四维比对」那条路。
- 别把 `/data/workspace/reverse-skill-quarantine/.../playbooks` 当成我们的剧本库读（那是隔离区的渗透剧本）。
- 历史经验实际散落在：`memory/agent/{cases,lessons,decisions,patterns}`（16 条）+ `CAPABILITIES/` 目录树本身 ——
  **后者才是现在真正可用的"剧本"**。
- 若要补建，优先从 `CAPABILITIES/01–06` 的「坑」段落反向回填，比从零写快。
