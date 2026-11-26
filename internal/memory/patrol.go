// Package memory 的简介巡检（线 B 第二阶段第 4 棒，v2.1 §6）：
// 核对磁盘原文、简介文件与主库三方一致，只发现、不生成简介，
// 绝不原地改写生产记忆文件（用户 2026-11-24 09:12 已取消巡检
// 原地改写；ScanDir 只做发现与统计）。
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"007-super-agent/internal/sanitize"
)

// 巡检用的状态值（与 internal/summary 的 Status 常量同值；
// memory 不能 import summary——summary 反过来依赖 memory 的主库，
// 会成环，故这里用同值字面常量）。
const (
	patrolStatusOK         = "ok"
	patrolStatusPending    = "pending"
	patrolStatusStale      = "stale"
	patrolStatusGenerating = "generating"
)

// 巡检常量：3 秒静默期（刚改完的文件下次再看）、generating 锁
// 5 分钟超时、细节行上限 200 条（账本再截断一次）、删档需连续
// 缺失次数（第 1 次只记数，第 2 次仍缺失再删，防文件原子替换
// 窗口误删；计数存在 patrol-state.json 里，全量也不丢）。
const (
	patrolQuietPeriod       = 3 * time.Second
	patrolLockTimeout       = 5 * time.Minute
	patrolMaxDetails        = 200
	patrolDeleteAfterMisses = 2
)

// PatrolOptions 是巡检的入参。
type PatrolOptions struct {
	Full bool      // 全量：忽略 state，全部重算哈希并重写 state
	Now  time.Time // 现在（为空用 time.Now；测试可注入）
}

// PatrolResult 是一次巡检的结果与计数。
type PatrolResult struct {
	Scanned           int      // 本次扫过的源文件数（含 state 命中跳过的）
	New               int      // 磁盘新增、主库无行的文件（只报告，不建行）
	Modified          int      // 源哈希变了的已入库文档
	Renamed           int      // 按哈希继承 doc_id 的改名/移动
	Deleted           int      // 源连续 2 次缺失、简介与库行同步删的文档
	DeleteDeferred    int      // 首见缺失、本次暂缓删的文档（下次仍缺失再删）
	StaleMarked       int      // 本次改标 stale 的篇数
	PendingMarked     int      // 本次改标 pending 的篇数
	SkippedGenerating int      // 跳过的 generating 文档（锁未超时）
	SecretFiles       int      // 含凭证串的文件数（只计数，不改文件）
	SecretHits        int      // 凭证命中次数合计
	Details           []string // 人读的细节行（账本与 CLI 共用，已截断）
	detailsSkipped    int      // 超过上限未列出的细节行数
}

// Summary 是一行巡检摘要（CLI 打印与日志用）。
func (r PatrolResult) Summary() string {
	s := fmt.Sprintf("巡检：扫 %d 篇，新增 %d，改动 %d，改名 %d，删除 %d，标 stale %d，标 pending %d，跳过 generating %d，含密钥文件 %d（%d 处）",
		r.Scanned, r.New, r.Modified, r.Renamed, r.Deleted, r.StaleMarked, r.PendingMarked, r.SkippedGenerating, r.SecretFiles, r.SecretHits)
	if r.DeleteDeferred > 0 {
		s += fmt.Sprintf("，暂缓删 %d（首见缺失）", r.DeleteDeferred)
	}
	return s
}

// addDetail 追加一条细节行，超过上限只计数。
func (r *PatrolResult) addDetail(s string) {
	if len(r.Details) >= patrolMaxDetails {
		r.detailsSkipped++
		return
	}
	r.Details = append(r.Details, s)
}

// finish 在返回前补一条“另 N 条未列出”（有截断时）。
func (r *PatrolResult) finish() {
	if r.detailsSkipped > 0 {
		r.Details = append(r.Details, fmt.Sprintf("…细节另 %d 条未列出", r.detailsSkipped))
	}
}

// patrolFileState 是巡检 state 里一个文件的一行（库文件旁的
// patrol-state.json）：mtime+size 预过滤，变了才算哈希；Missing
// 是连续缺失计数（源不在磁盘上时累加，在磁盘上重现时清零，
// 到 patrolDeleteAfterMisses 次才删简介与库行）。
type patrolFileState struct {
	Size    int64  `json:"size"`
	Mtime   int64  `json:"mtime"` // unix nano
	Hash    string `json:"hash"`
	Missing int    `json:"missing"`
}

type patrolDiskFile struct {
	Size  int64
	Mtime time.Time
}

// patrolStatePath 是巡检 state 的路径（主库文件旁）。
func patrolStatePath(st *SummaryStore) string {
	return filepath.Join(filepath.Dir(st.Path()), "patrol-state.json")
}

// summaryRelBySource 按布局从源相对路径推简介相对路径
// （简介/<层>/<类>/<同名文件>）。
func summaryRelBySource(sourceRel string) string {
	return filepath.Join("简介", filepath.FromSlash(sourceRel))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PatrolSummaryStore 跑一次简介巡检：核对磁盘原文、简介文件与主库。
// 只扫 wiki/ + l3/（简介只配这两层；l2/reference/l1 不扫）。
// 增量（Full=false）：读 patrol-state.json，mtime+size 未变跳过，
// 变了才算哈希；全量：忽略 state，全部重算并重写 state。
// 两种都只处理 now-mtime > 3 秒的文件（3 秒静默期）。
// 全新文件只记 New + 细节，不发明 doc_id（编号归 summary-ingest，
// 巡检自编号会撞号）。改名按 source_hash 唯一匹配继承 doc_id、
// 简介与触发词，只更新路径字段，并尽力移动简介文件。
// 巡检本身绝不调模型生成简介；简介缺失只标 pending/stale 进主库。
// 删除只在源连续 patrolDeleteAfterMisses 次巡检均缺失时执行
// （第 1 次只记数进 state，下次仍缺失再删），防精炼/自进化原子
// 替换文件的短暂缺失窗口造成误删；计数存在 patrol-state.json 里，
// 全量巡检重建 state 时也不丢。
func PatrolSummaryStore(memRoot string, st *SummaryStore, opts PatrolOptions) (PatrolResult, error) {
	var res PatrolResult
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	docs, err := st.ListDocs()
	if err != nil {
		return res, fmt.Errorf("巡检读主库失败: %w", err)
	}

	// 1. 先释放过期 generating 锁（锁超时 5 分钟 → stale），并更新内存副本，
	// 后面按正常流程处理这些文档；未超时的锁留在后面跳过。
	for i, d := range docs {
		if d.Status != patrolStatusGenerating {
			continue
		}
		lockedAt, perr := time.Parse(time.RFC3339, d.LockedAt)
		if perr == nil && now.Sub(lockedAt) <= patrolLockTimeout {
			continue
		}
		if err := st.ReleaseGenerating(d.DocID); err != nil {
			res.addDetail(fmt.Sprintf("释放过期锁失败：%s：%v", d.DocID, err))
			continue
		}
		res.StaleMarked++
		res.addDetail(fmt.Sprintf("锁超时释放：%s（%s）", d.DocID, d.SourcePath))
		docs[i].Status = patrolStatusStale
		docs[i].LockedAt = ""
	}

	bySource := make(map[string]SummaryDoc, len(docs))
	for _, d := range docs {
		bySource[d.SourcePath] = d
	}

	// 2. 扫磁盘：wiki/ + l3/ 下的 .md 文件。
	disk := make(map[string]patrolDiskFile)
	for _, layer := range []string{"wiki", "l3"} {
		root := filepath.Join(memRoot, layer)
		_ = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			if !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(memRoot, path)
			if err != nil {
				return nil
			}
			disk[filepath.ToSlash(rel)] = patrolDiskFile{Size: info.Size(), Mtime: info.ModTime()}
			return nil
		})
	}

	// 消失文档（源文件不在磁盘上），供改名匹配与删除用。
	missing := make(map[string]SummaryDoc)
	for _, d := range docs {
		if _, ok := disk[d.SourcePath]; !ok {
			missing[d.SourcePath] = d
		}
	}
	missingByHash := make(map[string][]SummaryDoc)
	for _, d := range missing {
		if d.SourceHash != "" {
			missingByHash[d.SourceHash] = append(missingByHash[d.SourceHash], d)
		}
	}

	// 3. 读巡检 state：增量预过滤用；全量也读，只为取上轮的缺失计数
	// （删档要连续 2 次缺失，全量重建 state 时也不能丢计数）。
	// 读坏当空 state，不阻塞。
	state := make(map[string]patrolFileState)
	if b, err := os.ReadFile(patrolStatePath(st)); err == nil {
		_ = json.Unmarshal(b, &state)
	}
	newState := make(map[string]patrolFileState, len(state)+len(disk))
	if !opts.Full {
		for k, v := range state {
			newState[k] = v
		}
	}

	renamedSources := make(map[string]bool)

	// 4. 逐文件处理。
	for rel, df := range disk {
		// 静默期：3 秒内改过的文件跳过（不更新 state，下次再看）。
		if now.Sub(df.Mtime) <= patrolQuietPeriod {
			continue
		}
		res.Scanned++
		// 增量预过滤：mtime+size 未变 → 跳过（不算哈希）。
		if !opts.Full {
			if prev, ok := state[rel]; ok && prev.Size == df.Size && prev.Mtime == df.Mtime.UnixNano() {
				continue
			}
		}
		data, err := os.ReadFile(filepath.Join(memRoot, filepath.FromSlash(rel)))
		if err != nil {
			res.addDetail(fmt.Sprintf("读失败跳过：%s：%v", rel, err))
			continue
		}
		hash := sha256Hex(data)

		if d, ok := bySource[rel]; ok {
			// 已有文档：generating 且锁未超时 → 跳过（第 1 步已释过期锁，
			// 到这里的 generating 都是未超时的）。注意：跳过时不写 state，
			// 否则锁释放后的下一轮会因 state 命中漏检本次改动。
			if d.Status == patrolStatusGenerating {
				res.SkippedGenerating++
				res.addDetail(fmt.Sprintf("跳过 generating：%s（%s）", d.DocID, rel))
				continue
			}
		}
		newState[rel] = patrolFileState{Size: df.Size, Mtime: df.Mtime.UnixNano(), Hash: hash}

		if d, ok := bySource[rel]; ok {
			// 源哈希变了 → 改动，标 stale。
			if d.SourceHash != "" && d.SourceHash != hash {
				res.Modified++
				if d.Status != patrolStatusStale {
					if err := st.SetStatus(d.DocID, patrolStatusStale); err != nil {
						res.addDetail(fmt.Sprintf("标 stale 失败：%s：%v", d.DocID, err))
					} else {
						res.StaleMarked++
					}
				}
				res.addDetail(fmt.Sprintf("源已改：%s（%s）", d.DocID, rel))
				continue
			}
			// 源没变：核对简介文件。
			sumRel := d.SummaryPath
			if sumRel == "" {
				sumRel = summaryRelBySource(rel)
			}
			sumData, err := os.ReadFile(filepath.Join(memRoot, filepath.FromSlash(sumRel)))
			if err != nil {
				if d.Status != patrolStatusPending {
					if err := st.SetStatus(d.DocID, patrolStatusPending); err != nil {
						res.addDetail(fmt.Sprintf("标 pending 失败：%s：%v", d.DocID, err))
					} else {
						res.PendingMarked++
					}
				}
				res.addDetail(fmt.Sprintf("缺简介：%s（%s）", d.DocID, rel))
				continue
			}
			if d.SummaryHash != "" && sha256Hex(sumData) != d.SummaryHash {
				if d.Status != patrolStatusStale {
					if err := st.SetStatus(d.DocID, patrolStatusStale); err != nil {
						res.addDetail(fmt.Sprintf("标 stale 失败：%s：%v", d.DocID, err))
					} else {
						res.StaleMarked++
					}
				}
				res.addDetail(fmt.Sprintf("简介已改：%s（%s）", d.DocID, rel))
				continue
			}
			// 全对：状态不是 ok 就恢复 ok（自愈：外部已补好简介）。
			if d.Status != patrolStatusOK {
				if err := st.SetStatus(d.DocID, patrolStatusOK); err != nil {
					res.addDetail(fmt.Sprintf("恢复 ok 失败：%s：%v", d.DocID, err))
				} else {
					res.addDetail(fmt.Sprintf("恢复 ok：%s（%s）", d.DocID, rel))
				}
			}
			continue
		}
		// 全新文件：改名匹配（哈希唯一命中一个消失文档）→ 继承；
		// 否则只记 New，不发明 doc_id。
		if cands := missingByHash[hash]; len(cands) == 1 {
			old := cands[0]
			newSumRel := summaryRelBySource(rel)
			if err := st.UpdatePaths(old.DocID, rel, newSumRel, hash); err != nil {
				res.addDetail(fmt.Sprintf("改名更新库失败：%s → %s：%v", old.SourcePath, rel, err))
				continue
			}
			// 尽力移动简介文件（失败只记，不阻塞）。
			if old.SummaryPath != "" {
				oldSumPath := filepath.Join(memRoot, filepath.FromSlash(old.SummaryPath))
				newSumPath := filepath.Join(memRoot, filepath.FromSlash(newSumRel))
				if _, err := os.Stat(oldSumPath); err == nil {
					_ = os.MkdirAll(filepath.Dir(newSumPath), 0o755)
					if err := os.Rename(oldSumPath, newSumPath); err != nil {
						res.addDetail(fmt.Sprintf("改名移简介失败：%s：%v", old.DocID, err))
					}
				}
			}
			delete(newState, old.SourcePath)
			res.Renamed++
			renamedSources[old.SourcePath] = true
			res.addDetail(fmt.Sprintf("改名继承：%s %s → %s", old.DocID, old.SourcePath, rel))
			continue
		}
		res.New++
		res.addDetail(fmt.Sprintf("新增未入库：%s（待外部写简介）", rel))
	}

	// 在磁盘上重现的文件清零缺失计数（下次缺失重新数；增量 state
	// 命中跳过的分支不写 newState，靠这里清零；静默期跳过的也一样）。
	for rel := range disk {
		if ent, ok := newState[rel]; ok && ent.Missing != 0 {
			ent.Missing = 0
			newState[rel] = ent
		}
	}

	// 5. 删除：源消失且未被改名继承 → 连续 2 次巡检均缺失才删简介文件
	// + DeleteDoc。第 1 次缺失只记数进 state（下次仍缺失再删），防精炼/
	// 自进化原子替换文件的短暂缺失窗口造成误删。
	// generating 且锁未超时的跳过（等锁超时后再处理，防误删）。
	for src, d := range missing {
		if renamedSources[src] {
			continue
		}
		if d.Status == patrolStatusGenerating {
			res.SkippedGenerating++
			res.addDetail(fmt.Sprintf("跳过 generating（源已消失，等锁超时）：%s（%s）", d.DocID, src))
			continue
		}
		misses := 1
		if prev, ok := state[src]; ok {
			misses = prev.Missing + 1
		}
		if misses < patrolDeleteAfterMisses {
			ent, _ := state[src]
			ent.Missing = misses
			newState[src] = ent
			res.DeleteDeferred++
			res.addDetail(fmt.Sprintf("源疑似消失（第 %d 次缺失，下次仍缺失再删）：%s（%s）", misses, d.DocID, src))
			continue
		}
		if d.SummaryPath != "" {
			_ = os.Remove(filepath.Join(memRoot, filepath.FromSlash(d.SummaryPath)))
		}
		if err := st.DeleteDoc(d.DocID); err != nil {
			res.addDetail(fmt.Sprintf("删库行失败：%s：%v", d.DocID, err))
			continue
		}
		delete(newState, src)
		res.Deleted++
		res.addDetail(fmt.Sprintf("源已删（连续 %d 次缺失）：%s（%s），简介与库行同步删", misses, d.DocID, src))
	}

	// 6. 密钥统计：只计数、不改文件、不输出命中值（ScanDir 本身只读）。
	// 只扫 wiki/ + l3/（与巡检范围一致；l1 日志里的密钥不计入）。
	for _, layer := range []string{"wiki", "l3"} {
		rep, err := sanitize.ScanDir(filepath.Join(memRoot, layer))
		if err != nil {
			res.addDetail(fmt.Sprintf("密钥扫描失败 %s：%v", layer, err))
			continue
		}
		res.SecretFiles += rep.FilesWithSecrets
		res.SecretHits += rep.TotalHits
	}

	// 7. 写回 state（增量更新改动项，全量重建；写失败只记，不阻塞）。
	if b, err := json.MarshalIndent(newState, "", "  "); err == nil {
		if err := os.WriteFile(patrolStatePath(st), b, 0o600); err != nil {
			res.addDetail(fmt.Sprintf("写巡检 state 失败：%v", err))
		}
	} else {
		res.addDetail(fmt.Sprintf("巡检 state 编码失败：%v", err))
	}

	res.finish()
	return res, nil
}
