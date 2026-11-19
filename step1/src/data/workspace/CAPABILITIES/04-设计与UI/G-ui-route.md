# `/ui` 路由（三段链路）
## 一句话
`/ui [--deep] [--write] <URL|路径> [附加说明]` → **detect 确定性打分 → 规则聚合 → pi 出修复包** 三段串行，纯只读（除非显式 `--write`），源码 `bot-gateway.js:471-540`。

## 实际效果（实测/读码 2026-10-26）
- **第 1 段 detect**：`run('…/scripts/impeccable',['detect','--json', target], {timeoutMs: deep?240000:150000, env:{IMPECCABLE_BROWSER: '/usr/local/bin/chrome-nosandbox', CHROME_PATH: 同值}})`
  - 参数只有 `--json`：**不传 `--viewport`**（永远桌面 1280x800，移动端问题扫不出来）、不传 `--scope`、不传 `--no-config`
  - `cwd` 缺省 = `/data/workspace`，URL 目标的 project root 就落在那里（现无 DESIGN.md，故无 design-system 比对）
- **第 2 段聚合**：按 `antipattern|severity` 去重计数，severity 排序 `error/critical < warning < info < advisory`，每条取 `snippet` 前 90 字组表；`JSON.parse` 失败一律 `findings=[]`
- **第 3 段 pi**：`runPi(prompt, deep?420000:180000)`，prompt 里带目标、聚合表、detect stderr 末 3 行、用户附加说明、输出格式（≤2500 字：结论 / 问题表 / 修复代码 / 验证命令 / 权限声明）
- 回复头：`🎨 <目标> 命中 N 条 / 去重 M 类 · 模式 quick|deep + 只读|写盘`，正文 `clip(pr.text, 9000)`，末尾带总耗时
- 实测各段真实量级：detect example.com 2.7s / github.com 28.7s；pi 段 3–5 分钟常见 → **quick 最坏 150+180=330s，deep 最坏 240+420=660s（11 分钟）**
- 全局 `enqueue` 串行队列：`/ui` 跑着的时候任何其它指令排队，不会并发

## 怎么调
```bash
# 等价的手工复跑（先只跑第 1 段，省钱省时）
export IMPECCABLE_BROWSER=/usr/local/bin/chrome-nosandbox
/root/.pi/agent/skills/impeccable/scripts/impeccable detect --json "https://example.com" | tail -c 2000
# 只读体检（本地项目）
/ui /data/workspace/mysite
# 深度：多读 critique.md + craft-floor.md，时间预算翻倍，检测规则不变
/ui --deep https://example.com
# 写盘（危险，见下）
/ui --write /data/workspace/mysite
```
- 想改超时/加参数（`--viewport`、`--scope`）→ 改 `bot-gateway.js:493-494`，改完 `node -c` 校验再重启网关
- 判定"到底是规则命中还是模型臆测"：只看回复头 `命中 N 条`，与第 1 段手工 detect 的条数对账

## 坑
- **`--deep` 名不副实**：detect 命令、参数、规则集**完全一样**，只是超时从 150→240s、pi 从 180→420s，外加让 pi 多读两篇 reference。规则不会变多。
- **静默假绿灯（最坑）**：detect 超时/失败（没设浏览器、目标 404、站点过重）→ stdout 空 → `findings=[]` → 头行写 `命中 0 条`，prompt 里写"（无命中：目标不可访问…）"。**"0 条"不等于"没问题"**，得看 `⏱ 总耗时` 是否 ≪2s（那时基本是 detect 秒失败）。
- `--write` 只在 prompt 第 5 条加一句"允许改目标源码，每个文件先复制 .bak 再写"——**网关不做备份、不做 diff、不校验**；而 `--write` 配 URL 目标也不报错（没东西可写，模型可能自己找路径落盘）。
- 参数用空白切分：路径带空格会被截断，`uNote` 会吃掉多余参数。
- `low.startsWith('/ui')` 是前缀匹配，`/uixxx` 也会进这条路由。
- 目标若是**非 HTML** 文件（CSS/JSX/TSX），只走 regex 匹配、不解析页面：引擎自己会警告 `tiny-text, low-contrast and other element rules did not run`，结果偏乐观。
