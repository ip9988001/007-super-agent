# 日志位置（/data/logs）

> **一句话**：查故障先看 `/data/logs/`，但**只有 4/6 个候选日志真的存在**，网关日志根本不落盘。

## 实际效果（实测）
| 文件 | 状态 | 实测大小 / 内容 |
|---|---|---|
| `boot.log` | ✅ | 7805 行 / 468K，逐段记录 `[boot] xxx 就绪`，查引导问题只看这个 |
| `l1-watcher.log` | ✅ | 4350 行 / 236K，含每 5s 心跳 `监控39个文件 \| L1已写: 5` |
| `l1-refine.log` | ✅ | 30 行 / 4K，全是「启动，等待每日 04:00」+ 一次被跳过的精炼 |
| `llmtrim.log` | ✅ | 49 行 / 4K，MITM 起停与 upstream 报错 |
| `bot-gateway.log` | ❌ **不存在** | 网关是 `exec node /data/bot-gateway.js` 前台进程，走 **stdout**（容器日志） |
| `claude-seo-setup.log` | ❌ **不存在** | 只有 `start.sh` 104 行「首次安装时才写」，本机从未走过那条分支 |

`/data/logs/` 里另有历史产物：`semgrep-install.log`(380K)、`rea-doctor*.json`(7 份)、
`reverse-skill-*.md`(4 份报告)、`chrome-cdp.log`、`codegraph-index.log`，以及三个大 jsonl（合计 34MB，可清）。

## 怎么调
```
tail -40 /data/logs/boot.log            # 引导结果（每段都有"就绪"行）
tail -f /data/logs/l1-watcher.log       # L1 采集是否活着（心跳 5s 一次）
grep -c "就绪" /data/logs/boot.log      # 数成功段
docker logs <ctr> --tail 100            # 网关日志只能从容器外拿
```

## 坑
- **网关没有任何落盘日志**：`bot-gateway.js` 用 `console.log('['+ts()+']', ...)` 全走 stdout，
  `/data/logs/bot-gateway.log` 是**想象中的文件**。容器重建 = 网关日志全灭，事故复盘只能靠 `sessions/*.jsonl`。
- 日志**不会轮转**：`boot.log` 已 7805 行、`semgrep-install.log` 380K，`audit73.jsonl` 这类批量产物 11–18MB 各一个，
  `deploy-rs.jsonl`(18MB) + `batch3.jsonl`(11MB) + `audit73.jsonl`(6.2MB) + `inspect-rs.jsonl`(4.7MB) 占 ~40MB，都是死数据。
- `l1-refine.log` 里 25+ 条「启动」是**容器重建次数**的化石，不是该进程自杀重启。
- 排查顺序建议：`boot.log`（有没有起来）→ 对应进程日志（活着没）→ `sessions/`（干了什么）。
