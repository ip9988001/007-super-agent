# cap-ops-lean-ctx

## 一句话
执行端压缩读网关（Go 原生 ctxread v3，2026-11-24 替换 Python 独立版）：读文件先给结构/签名再按需取行，24KB 封顶，纯只读。

## 怎么调
/root/super-agent-007/tools/ctxread <file> # 自动分档压缩读：<=8KB 全文；8-96KB 给结构图（imports/函数/类/标题）；>=96KB 给签名+行号；文档/数据文件 >24KB 给大纲；单行超长只留头 500 字；返回 24KB 封顶，附 token 估算
/root/super-agent-007/tools/ctxread lines <start> <end> <file> # 读指定行号窗口（end<=0 表示读到末尾），看完结构/签名后按行号取原文
/root/super-agent-007/tools/ctxread mode <mode> <file> # 强制分档：map / signatures / full / raw
/root/super-agent-007/tools/ctxread status # 探针：打印版本与 OK

## 坑
- 只读不写：只调 os.Stat 与 os.ReadFile，不写文件、不删改、不跑 shell 命令。
- 二进制文件、设备文件（/dev/zero）、命名管道拒绝读；不存在的文件返回退出码 1。
- 大于 10MB 的文件拒绝全量读，报错提示用脚本处理；ReadLines 同样受 10MB 限制。
- 输出 24KB 硬封顶（约 6000 token，对齐 E-12 R2）；超了自动截断并提示。
- 头文件（.h/.hpp）里的函数声明（以分号结尾）不进结构图，会退回前 40 行兜底。

## 实际效果
实测 2026-11-24：agent.go（41174 字节，838 行）→ map 约 572 tok（全文约 9083 tok，省约 94%）；96KB+ Go 文件签名带行号、泛型参数与类型别名；34KB CSV 走 map；单行 100KB 文件输出 631 字节。

## SOP
- 输入：任务目标 + 待处理文件路径。
- 步骤：1）先 `ctxread <大文件>` 看结构/签名；2）按行号 `ctxread lines <start> <end> <文件>` 取要看的原文段；3）核对输出与“实际效果”一致；4）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`ctxread status` → OK；`ctxread <大文件>` → 输出带结构/签名与 token 估算；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 24KB

来源：007 改革 2026-11-23 第 4 阶段（第 8 项，Python 独立版）· 2026-11-24 换 Go 原生 ctxread v3（按 Gemini v2 复审修过）· 核实：2026-11-24（Go 版实测）
> 注意：E-12 明令读文件走 ctxread；读文件先定预算 R0/R1/R2，超 8KB 必须走压缩读。
