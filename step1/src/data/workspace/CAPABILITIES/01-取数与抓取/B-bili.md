# bili（B 站 CLI）
## 一句话
bili 0.6.2 —— Bilibili 的命令行客户端：**只读**热门/排行/搜索/UP主/收藏/历史/动态，外加 `audio` 把视频音频下下来切成 ASR-ready WAV；点赞投币三连发动态等写操作要扫码登录。

## 实际效果（实测 2026-10-26）
- `bili --version` → `bili, version 0.6.2` ✅
- `bili status` → `ok: false` / `error.code: not_authenticated` / `message: 未登录。使用 bili login 登录。`（只读不受影响）
- `bili search 钢铁意志 --type video -n 2 --yaml` → ✅ 真数据：`BV1Qt411c7cz 【皮肤预览】重做后的钢铁意志 潘森`(play 219716) / `BV1E84y1f7oW （崩铁）卡芙卡boss战5个隐藏成就…`(play 88458)
- `bili hot -n 3 --yaml` → ✅ `BV12qnt6VEM1`「人性、欲望与奇观，自驾拉斯维加斯」aid `117489837016867`，带 description
- `bili hot --limit 3` ❌ `Error: No such option '--limit'.`（参数是 `-n/--max`）
- `bili audio --help` → 默认输出 `/tmp/bilibili-cli/{title}/`，每段 25 秒，16 kHz mono PCM WAV，`-s/--segment` 5~300 秒，`--no-split` 存完整 m4a

## 怎么调
```bash
bili hot -n 5 --yaml                    # 热门
bili rank --yaml                        # 全站排行
bili search 关键词 --type video -n 5 --yaml
bili user-videos <UID> -n 10 --yaml     # 某 UP 的视频列表
bili audio BV1ABcsztEcY -s 60 -o /data/tmp   # 下音频并切段（ASR 用）
bili --json …                           # 机器可读的另一种输出（与 --yaml 二选一）
```
写操作（都需要先 `bili login` 扫码）：`coin / like / triple / dynamic-post / dynamic-delete / unfollow`。

## 坑
- **只有 B 站内容**；要视频文件/字幕仍走 `Y-yt-dlp.md` 或 `A-agent-reach.md`（agent-reach 也直连 B 站，两者不冲突，bili 胜在"一键出音频段"）。
- 参数是 `-n/--max`，`--limit` 不存在（别凭直觉写）。
- 未登录时所有写操作都会 401 式失败，但读接口正常 —— 看到 `ok: false` 不必慌，检查一下是不是调了写命令。
- `audio` 默认落 `/tmp/bilibili-cli/`，**临时目录**，容器重启即丢；要留就 `-o` 到 `/data/`。
- `--yaml` 对 agent 更省 token（作者自己推荐），`--json` 给程序解析。
- 需要登录态的写操作本次**未实测**（会改账号状态），只用只读命令验证过。
