# ping0.cc 真机多步测试结果（第三轮，2026-11-18）

**结论：未通过（NOT PASS）。**规划器 + 核验器已跑通真模型，但执行层仍卡住两处，任务没做完，交付稿被监工拦下。

任务：ping0.cc 这个网站的所有功能都是怎么实现的，能不能复刻，复刻方案是什么（只做分析与方案，不实际搭建）。
任务 ID：ping0-test-20261118c；守护进程 PID 2526429（第 20+21 步 + B-04 修复 + 可执行清单修复后的二进制，md5 7842d45993d713e59d2b6642df95e8db）。
耗时：6 分 0 秒（06:10:55 接单 → 06:16:47 终局）。费用：277 毫分 = ¥0.277（三轮累计 442 毫分 ≈ ¥0.44）。

## 通过的部分

1. **规划器真跑通了**：14 步计划，核验器（reasoner）打回 2 轮（REVISE），最终执行修订版。规划质量高：curl 抓首页/robots/sitemap → 浏览器渲染 → rg 抽资源 → python 归一 → 批量抓 JS → rg 找接口 → python 解析 → GitHub 找同类开源 → jq 整理 → rg 功能词 → python 写报告 → python 验收，依赖关系合理。
2. **重规划修好了**：原计划第 2 步失败后，重规划成功产出 10 步新计划（r1），上一轮重规划是直接失败的。
3. **工作目录修复生效**：步骤 1 的 curl 全部成功（首页 200、robots 200、sitemap 404），4 个文件真实落盘到 data/workspace/ping0-test-20261118c/（home.headers、home.html、robots.txt、sitemap.xml）。上一轮的 /data/workspace 路径问题已解决。
4. **监工立功了**：交活前审 HALT，拦下了一份夸大其词的交付稿——稿子声称“步骤 1 打通连通性”，但账本里 r1 的两条 curl 都是 exit=-1，监工指出“失败结果被表述为成功”的矛盾，给出 6 条具体改法。这是监工第一次在真任务里拦下虚假交付。

## 没通过的原因

1. **浏览器渲染失败**：cap-fetch-browser-stack 两次 exit 1，`Missing X server or $DISPLAY`。两个病根：(a) 注册表 entry_point 是 `'chrome-nosandbox / playwright'`——带一个字面 `/` 的展示串，不是真实命令；(b) 包装脚本没有 --headless，测试机又没 DISPLAY（Xvfb 倒是有装，没用上）。
2. **r1 第 1 步 curl 两次 exit=-1、无错误信息**：同一条命令在测试机上手动跑两次都是 exit 0。不是确定性段错误（上一轮那个是 -D 指向不存在目录，这次目录存在），属瞬时性失败，原因未定位。重试一次也没过（两轮都 -1），随后因已用过一次重规划而停下。
3. **两版计划都没选 rea**：规划器这次走了 rg + python 静态分析路线，没挑 rea。策略本身合理，但“无提示词主动挑 rea”这项验证没过。
4. **核心问题没答出来**：没有渲染 DOM、没有 JS bundle、没有接口样本，功能清单只能靠常识推断，稿子里自己也标了“未实证”。监工拦下后终局状态为 delivery_halt，无正式交付。

## 诚实记录的两个小毛病

- 综合稿与监工都写了“没有落盘文件”，但原计划第 1 步的 4 个文件其实已落盘。原因是综合与监工的证据里没有工作目录文件清单，只看了 artifacts 目录。下一批修复要补。
- 费用大头在核验器：3 次 reasoner 调用 185 毫分（输出 13–16K tokens/次），占本轮 2/3。

## 下一批修复（待做）

1. 注册表 cap-fetch-browser-stack entry_point 改为 `tools/scripts/chrome-nosandbox`，并全库扫描 entry_point 里的展示串残留。
2. chrome-nosandbox 包装脚本加 `--headless=new`（种子 + 测试机两份都改）。
3. 综合与监工的证据包里加工作目录文件清单。
4. 第四轮重测。

---

# ping0.cc 真机多步测试结果（第四轮，2026-11-18）

**结论：未通过（NOT PASS）。**规划器写 shell 脚本当参数、执行器按 argv 传，curl 直接报 URL 非法；重规划后浏览器又卡在一条假 URL 上 11 分钟。

任务 ID：ping0-test-20261118d；守护进程 PID 2532468（step2021e，md5 433d394cb03840c674497e5dbac223bc）。
耗时：15 分钟轮询超时（06:43:02 接单，900 秒未终局）。费用：279 毫分 = ¥0.279（四轮累计 721 毫分 ≈ ¥0.72）。

## 经过

1. **计划 pl-2026-11-18-003**：12 步，REVISE，2 轮。步骤 1 仍是 shell 脚本当 inputs（`D=...; mkdir -p $D; curl ...; for ...; done | tee ...` 一整串），执行器把它当一个参数传给 curl，curl 两次 exit 3（`URL using bad/illegal format or missing URL`），8–11 毫秒即败。
2. **重规划 r1**：11 步，REVISE，0 轮，步骤写得比原计划干净（分开的 argv）。但第 1 步 inputs 仍是一整行 shell（`chrome-nosandbox --headless=new --dump-dom https://ping0.cc/ > .../01-home-dom.html`），执行器传给包装脚本一个参数，Chrome 收到后把整串当 URL，卡住 660 秒不动（CPU 只用了 1 秒，纯等）。我手动 kill 掉 Chrome 进程组才解开——Chrome 收 SIGTERM 会干净退出 0，账本记成 success（空 stdout、无落盘文件），这是我外部干预造成的假成功，不是执行器本身的 bug（执行器自己的 1800 秒超时走的是 Killed=true、exit -1、failure）。
3. **r1 第 2 步 playwright**：两次 exit 1，`error: unknown option '--out'`——模型编了个不存在的参数。
4. 终局：inbox DONE（失败结局），无 artifacts，工作目录空。核验器 4 次调用 243 毫分，占本轮近九成。

## 根因

1. **规划器提示词没讲清 argv 契约**：只写了“执行时不经过 shell”，没说每个 inputs 元素就是一个命令行参数、不许写管道/重定向/分号/$变量/for 循环、不许在 inputs 里重复工具名。模型照着 shell 习惯写。
2. **浏览器无超时兜底**：Chrome 卡在假 URL 上能等满执行器的 1800 秒。
3. **模型编参数**：playwright screenshot 没有 --out，模型没先跑 --help 确认。

## 修复（已部署，step2021g）

1. 规划器提示词加 argv 契约（每个字符串一个参数，不许 shell 语法、不许重复工具名、路径写绝对路径、落盘用 --out 参数不用重定向）+“不确定的命令先跑 --help 确认用法，不要编参数”。
2. 核验器提示词 rationality 一项加 argv 检查（含量 shell 语法的步骤一律打回）。
3. chrome-nosandbox 包装脚本加 `--timeout=60000`（实测：正常页 0.9 秒出 DOM，不等满 60 秒；挂起的 URL 5 秒退出）。
4. 注册表 v1.5（chrome-nosandbox 一行说明补超时）。

---

# ping0.cc 真机多步测试结果（第五轮，2026-11-18）

**结论：链路通过、证据没过（CHAIN PASS, EVIDENCE FAIL）。**提示词修复生效，参数全是干净的 argv，任务跑完且交活诚实；但浏览器渲染一步没跑成，分析没拿到实证，复刻方案没产出。

任务 ID：ping0-test-20261118e；守护进程 PID 2536001（step2021g，md5 0a13cea869214b53659b740466d55989）。
耗时：7 分 0 秒（07:01:50 接单 → 07:08:50 终局 DONE）。费用：354 毫分 = ¥0.354（五轮累计 1075 毫分 ≈ ¥1.08）。

## 通过的部分

1. **argv 契约生效**：计划 pl-2026-11-18-004（12 步，REVISE，2 轮）的 inputs 全是分开的参数，如 curl `["-sS","-L","--max-time","30","-D","/绝对路径/headers.txt","-o","/绝对路径/home.html","https://ping0.cc"]`，无 shell 语法、无 $变量、无重定向。
2. **--help 先行生效**：模型按新规则先跑 `--help` 确认用法（browser-stack --help、lightpanda --help），playwright 第 5 步改用位置参数传截图路径，不再编 --out。
3. **链路跑完**：curl 成功、lightpanda 成功、python 12 步成功，任务 DONE（420 秒），artifacts 有 reply.md + 9 个步骤文件，report.md 已生成。
4. **交活诚实**：reply.md 明确写“分析未完成、复刻方案未产出”，列出卡点，不编结论。

## 没通过的原因

1. **browser-stack --help 两次 exit 16**：`No manual entry for chrome`。Chrome 在无头模式下 --help 会去找 man 页，测试机没装 man 页。原计划第 1 步（--help）失败触发重规划，r1（9 步）干脆丢掉了浏览器渲染，改走 lightpanda。
2. **无渲染 DOM**：home.html 仅 40 字节（ping0.cc 首屏是空壳，靠 JS 填充），lightpanda 抓回的也是空内容，python 侧 assets 0、endpoints 0、probed 0、pages 0。report.md（1505 字节）基于近乎空的数据，复刻方案无从写起。
3. **模型仍在 inputs 里重复工具名**：原计划第 4 步 inputs 是 `["chrome-nosandbox","--headless=new","--dump-dom","https://ping0.cc"]`，首元素就是工具名（照 L3 整行命令抄的）。提示词已禁，但模型还是写了一次。

## 修复（已部署，step2021h）

1. chrome-nosandbox 包装脚本拦截 --help/-h：直接打印用法、exit 0（测试机 + 种子 + 公开库三处都更新）。
2. BuildArgv 加防呆：首参数与命令名重复时去掉一个（回归测试已加）。
3. 第六轮重测中（任务 ping0-test-20261118f，守护进程 PID 2537437，step2021h）。

---

# ping0.cc 真机多步测试结果（第六轮，2026-11-18）

**结论：未通过（NOT PASS）——新故障：核验器回了非法 JSON，整个任务 90 秒就被杀掉。**链路本身没跑起来，无计划、无执行、无产物。

任务 ID：ping0-test-20261118f；守护进程 PID 2537437（step2021h，md5 d27c23d79c25a7a48e558c2617111dfd）。
耗时：1 分 30 秒（07:11:05 接单 → 07:12:35 终局 DONE）。费用：68 毫分 = ¥0.068（六轮累计 1143 毫分 ≈ ¥1.14）。

## 经过

1. 07:11:10 triage（flash 1 毫分）。
2. 07:11:51 规划器调 flash 出计划（32 毫分）。
3. 07:12:35 核验器调 reasoner（35 毫分），回来的 JSON 是坏的——守护进程日志：`task=ping0-test-20261118f plan failed: planner verify: planner: 核验回复非法: invalid character 'q' after object key`。
4. 计划、执行、产物全部没有，inbox 直接 DONE。账本里 plan_runs、execution 均为空。

## 根因

核验器的 JSON 解析是严格的、单次的：模型偶尔返回非法 JSON（reasoner 是非确定性的），`parseVerification` 一失败，`Verify` 直接返回错误，`PlanAndVerify` 直接杀任务。一次坏回复就毁掉整个任务，没有重试、没有兜底。

## 修复（已部署，step2021i）

1. `parseVerification` 加回退：直接解析失败时，抠出最外层 JSON 对象（第一个 `{` 到最后一个 `}`）再试一次——模型常在 JSON 前后加解释文字。
2. `Verify` 加重试：解析失败时重调模型，最多 2 次，重调时提醒它“只返回 JSON，不要加解释文字”。
3. `PlanAndVerify` 加兜底：核验器三轮都失败也不杀任务，按原计划执行，但记一笔“核验不可用”（工具均已过代码实核，不可逆动作仍有监工把关）。
4. 三个回归测试已加（抠 JSON、重试、兜底），全套 15 包测试通过。代码已推公开库（ec0a77f）与私有库（1025cc6）。
5. 第七轮重测中（任务 ping0-test-20261118g，守护进程 PID 2537959，step2021i）。

---

# ping0.cc 真机多步测试结果（第七轮，2026-11-18）

**结论：链路已稳、证据仍薄（CHAIN PASS, EVIDENCE FAIL）。**JSON 兜底生效，计划正常产出；浏览器 --help 也通了，dump-dom 成功返回；但证据还是空壳——根因已定位：ping0.cc 有 Cloudflare Turnstile 验证。

任务 ID：ping0-test-20261118g；守护进程 PID 2537959（step2021i，md5 c9055cd2b8266e8f28a3421a11a87966）。
耗时：6 分 40 秒（07:14:53 接单 → 07:21:33 终局 DONE）。费用：344 毫分 = ¥0.344（七轮累计 1487 毫分 ≈ ¥1.49）。

## 通过的部分

1. **JSON 兜底生效**：计划 pl-2026-11-18-005（9 步，REVISE，2 轮）正常产出，无“plan failed”。核验器 5 次调用全部通过。
2. **浏览器 --help 通了**：browser-stack `--headless=new --dump-dom https://ping0.cc/` exit 0，BuildArgv 防呆也生效（模型这次没再重复工具名）。
3. **规划器选工具变聪明了**：先 agent-reach --help，再 curl，再 agent-reach web，失败后重规划改走 browser-stack + python。inputs 全是干净 argv。
4. **交活诚实**：reply.md 明确写 analysis.md 是“推断性方案”，不是实证拆解；analysis.md 里的证据（nginx、Cloudflare DNS、MX、3 条探测）都是真的。

## 没通过的原因

1. **agent-reach web 两次 exit 1**（`ok: false`），渲染失败。
2. **browser-stack dump-dom 只拿到空壳**（40 字节），assets 0，无 JS bundle。
3. **根因已定位（我手动查的）**：ping0.cc 有 Cloudflare Turnstile 验证。只带 UA 的请求回 40 字节空壳；带全套浏览器头回 1357 字节的验证页（Turnstile + 阿里云验证码，解完才 set cookie 刷新进真站）。普通抓取、普通无头浏览器都过不去，必须用反检测工具（camoufox、scrapling stealthy_fetch）。

## 修复（已部署，step2021j，注册表 v1.6）

1. 规划器提示词加一条：抓 HTML 带全套浏览器头（只带 UA 常被回空壳/验证页）；拿到验证页或空壳时换反检测工具（camoufox、scrapling stealthy_fetch），不要在空壳上反复试普通抓取。
2. 注册表 3 条 one-liner 改写（scrapling、camoufox、curl-jq），点明“绕过 Cloudflare Turnstile/机器人验证”与“全套浏览器头”。
3. 代码已推公开库（da0a085）与私有库（960f755）。
4. 第八轮重测中（任务 ping0-test-20261118h，守护进程 PID 2539766，step2021j）——看规划器会不会主动挑反检测工具。
