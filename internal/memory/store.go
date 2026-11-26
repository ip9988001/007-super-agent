// Package memory 的简介主库（线 B 第二阶段第 3 棒）：单文件 SQLite，
// WAL 模式。表结构按 v2.1 §5：docs 主表 + FTS5 全文索引（对
// summary + triggers）+ doc_vectors 向量表（简介向量 only，不建
// 全文向量）。
//
// 向量选型（2026-11-24 服务器实测）：
//   - asg017/sqlite-vec-go-bindings v0.1.6 CGO 版编不过：缺 sqlite3.h
//     （服务器无 libsqlite3-dev），且 README 明言 CGO 版不兼容本项目
//     用的 modernc.org/sqlite，只支持 mattn/go-sqlite3；
//   - ncruces（WASM）版也编不过：与最新 ncruces/go-sqlite3 版本错位，
//     报错 undefined: sqlite3.Binary，且它要求整个库换用 ncruces
//     驱动（非 database/sql 接口），与账本、旧向量库的 modernc
//     驱动混用会变成双 SQLite 引擎 + WASM 运行时；
//   - 结论：向量存 BLOB（doc_vectors.vec，小端 float32）+ Go 内暴力
//     余弦。1693 篇 × 1024 维 ≈ 170 万次乘加，单次查询毫秒级，
//     无新驱动、无 CGO，跨平台编译故事（Linux/macOS 交叉编译）
//     不受影响。规模上万后再评估 sqlite-vec。
//
// JSONL/Markdown 只作导出物（ExportJSONL / ExportMarkdown），
// 本棒先把导出函数写好，不强制定时跑。
package memory

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SummaryDoc 是主库里的一篇简介（v2.1 §5 字段 + 标题/简介正文，
// 召回直接用，不再回读简介文件）。
type SummaryDoc struct {
	DocID       string `json:"doc_id"`
	Layer       string `json:"layer"`
	Category    string `json:"category"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	Triggers    string `json:"triggers"` // 触发词，逗号分隔原文
	SummaryPath string `json:"summary_path"`
	SourcePath  string `json:"source_path"`
	SourceHash  string `json:"source_hash"`
	SummaryHash string `json:"summary_hash"`
	Status      string `json:"status"`
	RetryCount  int    `json:"retry_count"`
	// SupersededBy 是取代本篇的新文档 doc_id（v2.1 §7）：空表示现行，
	// 非空表示本篇已过期。由人工/后续流程填，不自动推断。
	SupersededBy string `json:"superseded_by"`
	// LockedAt 是 generating 锁的置位时间（RFC3339 UTC）：非空表示
	// 本篇正由外部生成简介，巡检跳过；超过 5 分钟未释放视为超时，
	// 巡检自动释放并改标 stale。空表示无锁。
	LockedAt  string `json:"locked_at"`
	UpdatedAt string `json:"updated_at"`
}

// SummaryStore 是简介主库：SQLite（WAL）+ FTS5 + 向量 BLOB。
type SummaryStore struct {
	dbPath string
	db     *sql.DB
}

const summarySchema = `
CREATE TABLE IF NOT EXISTS docs (
  doc_id TEXT PRIMARY KEY,
  layer TEXT NOT NULL,
  category TEXT NOT NULL,
  title TEXT NOT NULL,
  summary TEXT NOT NULL,
  triggers TEXT NOT NULL DEFAULT '',
  summary_path TEXT NOT NULL,
  source_path TEXT NOT NULL UNIQUE,
  source_hash TEXT NOT NULL,
  summary_hash TEXT NOT NULL,
  status TEXT NOT NULL,
  retry_count INTEGER NOT NULL DEFAULT 0,
  superseded_by TEXT NOT NULL DEFAULT '',
  locked_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS doc_vectors (
  doc_id TEXT PRIMARY KEY REFERENCES docs(doc_id) ON DELETE CASCADE,
  dim INTEGER NOT NULL,
  vec BLOB NOT NULL,
  model TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- FTS5 全文索引（对 summary + triggers；search_text 是 bigram
-- 展开后的检索文本，见 ftsSearchText：FTS5 unicode61 分词把中文
-- 连续串当一个 token，直接 MATCH 中文词查不到，故索引时另存
-- 二字滑窗展开文本，查询侧同样展开后再 MATCH）。
CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(
  title, summary, triggers, search_text, tokenize='unicode61'
);
`

// OpenSummaryStore 打开（不存在则创建）简介主库并建表，开 WAL。
// path 为空返回错误；调用方负责 Close。
func OpenSummaryStore(path string) (*SummaryStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("summary store: 路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("summary store mkdir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("summary store open: %w", err)
	}
	// 单连接串行写，避免 WAL 下多连接写冲突；读也走同一连接，简单可靠。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("summary store wal: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("summary store sync: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("summary store busy_timeout: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		db.Close()
		return nil, fmt.Errorf("summary store fk: %w", err)
	}
	// 迁移（线 B 第二阶段）：旧库的 docs 没有 locked_at 列（generating
	// 锁），先补列再建表；表不存在（新库）时跳过，建表语句自带该列。
	{
		var hasTable int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'docs'`).Scan(&hasTable); err != nil {
			db.Close()
			return nil, fmt.Errorf("summary store migrate check docs table: %w", err)
		}
		if hasTable > 0 {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('docs') WHERE name = 'locked_at'`).Scan(&n); err != nil {
				db.Close()
				return nil, fmt.Errorf("summary store migrate check locked_at: %w", err)
			}
			if n == 0 {
				if _, err := db.Exec(`ALTER TABLE docs ADD COLUMN locked_at TEXT NOT NULL DEFAULT ''`); err != nil {
					db.Close()
					return nil, fmt.Errorf("summary store migrate add locked_at: %w", err)
				}
			}
		}
	}
	if _, err := db.Exec(summarySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("summary store schema: %w", err)
	}
	return &SummaryStore{dbPath: path, db: db}, nil
}

// Path 返回库文件路径。
func (s *SummaryStore) Path() string { return s.dbPath }

// Close 关闭主库。
func (s *SummaryStore) Close() error { return s.db.Close() }

// JournalMode 返回当前 journal_mode（测试断言 WAL 用）。
func (s *SummaryStore) JournalMode() (string, error) {
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		return "", fmt.Errorf("summary store journal_mode: %w", err)
	}
	return mode, nil
}

// UpsertDoc 写入或更新一篇简介（按 doc_id 幂等：复提复用 doc_id，
// 不重号；source_path 换主时旧 FTS 行一并清掉）。不写向量，
// 向量另走 UpsertVector。调用方须保证 Summary/Title 已质检。
func (s *SummaryStore) UpsertDoc(d SummaryDoc) error {
	if d.DocID == "" || d.SourcePath == "" || d.Summary == "" {
		return fmt.Errorf("summary store upsert: doc_id/source_path/summary 不可为空")
	}
	if d.UpdatedAt == "" {
		d.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("summary store upsert begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM docs_fts WHERE rowid IN
		(SELECT rowid FROM docs WHERE doc_id=?)`, d.DocID); err != nil {
		return fmt.Errorf("summary store upsert fts 清旧: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO docs
		(doc_id, layer, category, title, summary, triggers, summary_path, source_path,
		 source_hash, summary_hash, status, retry_count, superseded_by, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(doc_id) DO UPDATE SET
		 layer=excluded.layer, category=excluded.category, title=excluded.title,
		 summary=excluded.summary, triggers=excluded.triggers,
		 summary_path=excluded.summary_path, source_path=excluded.source_path,
		 source_hash=excluded.source_hash, summary_hash=excluded.summary_hash,
		 status=excluded.status, retry_count=excluded.retry_count,
		 superseded_by=excluded.superseded_by, locked_at='',
		 updated_at=excluded.updated_at`,
		d.DocID, d.Layer, d.Category, d.Title, d.Summary, d.Triggers,
		d.SummaryPath, d.SourcePath, d.SourceHash, d.SummaryHash,
		d.Status, d.RetryCount, d.SupersededBy, d.UpdatedAt); err != nil {
		return fmt.Errorf("summary store upsert docs: %w", err)
	}
	var rowid int64
	if err := tx.QueryRow(`SELECT rowid FROM docs WHERE doc_id=?`, d.DocID).Scan(&rowid); err != nil {
		return fmt.Errorf("summary store upsert rowid: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO docs_fts(rowid, title, summary, triggers, search_text)
		VALUES(?,?,?,?,?)`, rowid, d.Title, d.Summary, d.Triggers, ftsSearchText(d)); err != nil {
		return fmt.Errorf("summary store upsert fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("summary store upsert commit: %w", err)
	}
	return nil
}

// UpsertVector 写入一篇简介的向量（BLOB，小端 float32；见
// semantic.go encodeVec）。doc_id 须已在 docs 表里。
func (s *SummaryStore) UpsertVector(docID string, vec []float32, model string) error {
	if docID == "" || len(vec) == 0 {
		return fmt.Errorf("summary store upsert vector: doc_id/向量不可为空")
	}
	if model == "" {
		model = SemanticDefaultModel
	}
	_, err := s.db.Exec(`INSERT INTO doc_vectors(doc_id, dim, vec, model, updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(doc_id) DO UPDATE SET dim=excluded.dim, vec=excluded.vec,
		 model=excluded.model, updated_at=excluded.updated_at`,
		docID, len(vec), encodeVec(vec), model, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("summary store upsert vector %s: %w", docID, err)
	}
	return nil
}

// GetDoc 按 doc_id 取一篇简介；不存在返回 sql.ErrNoRows 的包装错误。
func (s *SummaryStore) GetDoc(docID string) (SummaryDoc, error) {
	var d SummaryDoc
	err := s.db.QueryRow(`SELECT doc_id, layer, category, title, summary, triggers,
		summary_path, source_path, source_hash, summary_hash, status, retry_count,
		superseded_by, locked_at, updated_at FROM docs WHERE doc_id=?`, docID).Scan(
		&d.DocID, &d.Layer, &d.Category, &d.Title, &d.Summary, &d.Triggers,
		&d.SummaryPath, &d.SourcePath, &d.SourceHash, &d.SummaryHash, &d.Status,
		&d.RetryCount, &d.SupersededBy, &d.LockedAt, &d.UpdatedAt)
	if err != nil {
		return d, fmt.Errorf("summary store get %s: %w", docID, err)
	}
	return d, nil
}

// GetBySource 按源文件相对路径取一篇简介。
func (s *SummaryStore) GetBySource(sourcePath string) (SummaryDoc, error) {
	var d SummaryDoc
	err := s.db.QueryRow(`SELECT doc_id, layer, category, title, summary, triggers,
		summary_path, source_path, source_hash, summary_hash, status, retry_count,
		superseded_by, locked_at, updated_at FROM docs WHERE source_path=?`, sourcePath).Scan(
		&d.DocID, &d.Layer, &d.Category, &d.Title, &d.Summary, &d.Triggers,
		&d.SummaryPath, &d.SourcePath, &d.SourceHash, &d.SummaryHash, &d.Status,
		&d.RetryCount, &d.SupersededBy, &d.LockedAt, &d.UpdatedAt)
	if err != nil {
		return d, fmt.Errorf("summary store get source %s: %w", sourcePath, err)
	}
	return d, nil
}

// ListDocs 返回全部简介（按 doc_id 升序）。
func (s *SummaryStore) ListDocs() ([]SummaryDoc, error) {
	rows, err := s.db.Query(`SELECT doc_id, layer, category, title, summary, triggers,
		summary_path, source_path, source_hash, summary_hash, status, retry_count,
		superseded_by, locked_at, updated_at FROM docs ORDER BY doc_id`)
	if err != nil {
		return nil, fmt.Errorf("summary store list: %w", err)
	}
	defer rows.Close()
	var out []SummaryDoc
	for rows.Next() {
		var d SummaryDoc
		if err := rows.Scan(&d.DocID, &d.Layer, &d.Category, &d.Title, &d.Summary,
			&d.Triggers, &d.SummaryPath, &d.SourcePath, &d.SourceHash, &d.SummaryHash,
			&d.Status, &d.RetryCount, &d.SupersededBy, &d.LockedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("summary store list scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("summary store list rows: %w", err)
	}
	return out, nil
}

// CountDocs 返回简介篇数。
func (s *SummaryStore) CountDocs() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM docs`).Scan(&n); err != nil {
		return 0, fmt.Errorf("summary store count docs: %w", err)
	}
	return n, nil
}

// CountVectors 返回已有向量的篇数。
func (s *SummaryStore) CountVectors() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM doc_vectors`).Scan(&n); err != nil {
		return 0, fmt.Errorf("summary store count vectors: %w", err)
	}
	return n, nil
}

// SetStatus 只改一篇的状态（巡检标记 stale/pending/ok 用；不碰哈希与锁）。
func (s *SummaryStore) SetStatus(docID, status string) error {
	if _, err := s.db.Exec(`UPDATE docs SET status=?, updated_at=? WHERE doc_id=?`,
		status, time.Now().UTC().Format(time.RFC3339), docID); err != nil {
		return fmt.Errorf("summary store status %s: %w", docID, err)
	}
	return nil
}

// SetGenerating 置 generating 锁：状态改 generating，locked_at 写置位时间
// （RFC3339 UTC；空串用当前时间）。供简介生成任务进入时调用。
// 生成方入库后不用手动清锁：UpsertDoc 入库时自动清 locked_at。
// 生成方中途失败也不用管：巡检在锁超过 5 分钟后自动释放并改标 stale。
func (s *SummaryStore) SetGenerating(docID, lockedAt string) error {
	if lockedAt == "" {
		lockedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if _, err := s.db.Exec(`UPDATE docs SET status='generating', locked_at=?,
		updated_at=? WHERE doc_id=?`, lockedAt,
		time.Now().UTC().Format(time.RFC3339), docID); err != nil {
		return fmt.Errorf("summary store generating %s: %w", docID, err)
	}
	return nil
}

// ReleaseGenerating 释放过期锁：状态改 stale（简介待补，等外部重写），
// locked_at 清空。只供巡检在锁超时后调用。
func (s *SummaryStore) ReleaseGenerating(docID string) error {
	if _, err := s.db.Exec(`UPDATE docs SET status='stale', locked_at='',
		updated_at=? WHERE doc_id=?`,
		time.Now().UTC().Format(time.RFC3339), docID); err != nil {
		return fmt.Errorf("summary store release generating %s: %w", docID, err)
	}
	return nil
}

// UpdatePaths 只更新一篇的路径与源哈希（改名/移动继承 doc_id 时用；
// 不碰简介内容、触发词与状态）。
func (s *SummaryStore) UpdatePaths(docID, sourcePath, summaryPath, sourceHash string) error {
	if _, err := s.db.Exec(`UPDATE docs SET source_path=?, summary_path=?,
		source_hash=?, updated_at=? WHERE doc_id=?`, sourcePath, summaryPath,
		sourceHash, time.Now().UTC().Format(time.RFC3339), docID); err != nil {
		return fmt.Errorf("summary store paths %s: %w", docID, err)
	}
	return nil
}

// DeleteDoc 删一篇简介：docs 行 + 向量行 + FTS 行，同一事务内完成。
// 简介文件本身由调用方（巡检）删，这里只清库。
func (s *SummaryStore) DeleteDoc(docID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("summary store delete begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM docs_fts WHERE rowid IN
		(SELECT rowid FROM docs WHERE doc_id=?)`, docID); err != nil {
		return fmt.Errorf("summary store delete fts %s: %w", docID, err)
	}
	if _, err := tx.Exec(`DELETE FROM doc_vectors WHERE doc_id=?`, docID); err != nil {
		return fmt.Errorf("summary store delete vector %s: %w", docID, err)
	}
	if _, err := tx.Exec(`DELETE FROM docs WHERE doc_id=?`, docID); err != nil {
		return fmt.Errorf("summary store delete docs %s: %w", docID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("summary store delete commit: %w", err)
	}
	return nil
}

// GetByHash 按源文件哈希取简介（哈希可能撞——内容相同的两篇会同哈希；
// 返回全部匹配行，调用方自行判断唯一性）。
func (s *SummaryStore) GetByHash(sourceHash string) ([]SummaryDoc, error) {
	rows, err := s.db.Query(`SELECT doc_id, layer, category, title, summary, triggers,
		summary_path, source_path, source_hash, summary_hash, status, retry_count,
		superseded_by, locked_at, updated_at FROM docs WHERE source_hash=? ORDER BY doc_id`, sourceHash)
	if err != nil {
		return nil, fmt.Errorf("summary store by hash: %w", err)
	}
	defer rows.Close()
	var out []SummaryDoc
	for rows.Next() {
		var d SummaryDoc
		if err := rows.Scan(&d.DocID, &d.Layer, &d.Category, &d.Title, &d.Summary,
			&d.Triggers, &d.SummaryPath, &d.SourcePath, &d.SourceHash, &d.SummaryHash,
			&d.Status, &d.RetryCount, &d.SupersededBy, &d.LockedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("summary store by hash scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("summary store by hash rows: %w", err)
	}
	return out, nil
}

// ListByStatus 按状态取简介（巡检扫 generating 锁用）。
func (s *SummaryStore) ListByStatus(status string) ([]SummaryDoc, error) {
	rows, err := s.db.Query(`SELECT doc_id, layer, category, title, summary, triggers,
		summary_path, source_path, source_hash, summary_hash, status, retry_count,
		superseded_by, locked_at, updated_at FROM docs WHERE status=? ORDER BY doc_id`, status)
	if err != nil {
		return nil, fmt.Errorf("summary store by status: %w", err)
	}
	defer rows.Close()
	var out []SummaryDoc
	for rows.Next() {
		var d SummaryDoc
		if err := rows.Scan(&d.DocID, &d.Layer, &d.Category, &d.Title, &d.Summary,
			&d.Triggers, &d.SummaryPath, &d.SourcePath, &d.SourceHash, &d.SummaryHash,
			&d.Status, &d.RetryCount, &d.SupersededBy, &d.LockedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("summary store by status scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("summary store by status rows: %w", err)
	}
	return out, nil
}

// SetSupersededBy 标记一篇旧文档被新文档取代（v2.1 §7）：
// superseded_by 记新文档的 doc_id；清空传 ""。
func (s *SummaryStore) SetSupersededBy(docID, newDocID string) error {
	if _, err := s.db.Exec(`UPDATE docs SET superseded_by=?,
		updated_at=? WHERE doc_id=?`, newDocID,
		time.Now().UTC().Format(time.RFC3339), docID); err != nil {
		return fmt.Errorf("summary store superseded %s: %w", docID, err)
	}
	return nil
}

// SetMeta 写一条 meta（模型名、建成时间等）。
func (s *SummaryStore) SetMeta(key, value string) error {
	if _, err := s.db.Exec(`INSERT INTO meta(key, value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
		return fmt.Errorf("summary store meta %s: %w", key, err)
	}
	return nil
}

// GetMeta 读一条 meta；不存在返回 ""。
func (s *SummaryStore) GetMeta(key string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v); err != nil {
		return ""
	}
	return v
}

// ---- FTS5 ----

// ftsHit 是 FTS5 简介层的一条命中：bm25() 返回负值，越小越相关，
// 这里转成正分（取负）供调用方比较。
type ftsHit struct {
	DocID string
	Score float64 // bm25 正分，越大越相关
}

// ftsSearchText 生成 FTS5 索引用的检索文本：原文 + 中文二字滑窗
// 展开（FTS5 unicode61 把中文连续串当一个 token，不展开查不到）。
func ftsSearchText(d SummaryDoc) string {
	var b strings.Builder
	b.WriteString(d.Title)
	b.WriteString("\n")
	b.WriteString(d.Summary)
	b.WriteString("\n")
	b.WriteString(d.Triggers)
	b.WriteString("\n")
	for _, t := range bm25Terms(d.Title + " " + d.Summary + " " + d.Triggers) {
		b.WriteString(t)
		b.WriteString(" ")
	}
	return b.String()
}

// ftsMatchExpr 把查询词拼成 FTS5 MATCH 表达式：每词双引号包起来
// 做短语匹配，词间 OR。空词表返回 ""（调用方不查 FTS）。
func ftsMatchExpr(terms []string) string {
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		t = strings.ReplaceAll(t, `"`, `""`)
		parts = append(parts, `"`+t+`"`)
	}
	return strings.Join(parts, " OR ")
}

// SearchFTS 是简介层词法腿：FTS5 bm25 检索，b=0.30（v2.1 §7
// 简介层 b=0.25–0.35）。列权重：标题 2、简介 1、触发词 1、
// 展开文本 0.5；k1=1.2。返回按相关度降序的前 limit 条。
// terms 是已展开的查询词（缩写展开 + 中文二字滑窗，见 bm25Terms）。
func (s *SummaryStore) SearchFTS(terms []string, limit int) ([]ftsHit, error) {
	expr := ftsMatchExpr(terms)
	if expr == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT d.doc_id, -bm25(docs_fts, 2.0, 1.0, 1.0, 0.5, 1.2, 0.30) AS s
		 FROM docs_fts JOIN docs d ON d.rowid = docs_fts.rowid
		 WHERE docs_fts MATCH ? ORDER BY s DESC LIMIT ?`, expr, limit)
	if err != nil {
		return nil, fmt.Errorf("summary store fts: %w", err)
	}
	defer rows.Close()
	var out []ftsHit
	for rows.Next() {
		var h ftsHit
		if err := rows.Scan(&h.DocID, &h.Score); err != nil {
			return nil, fmt.Errorf("summary store fts scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("summary store fts rows: %w", err)
	}
	return out, nil
}

// storedVector 是库里的一条简介向量（暴力余弦用）。
type storedVector struct {
	DocID    string
	Layer    string
	Category string
	Vec      []float32
}

// LoadVectors 读出全部简介向量（供语义腿暴力余弦；1693 篇规模
// 下全量进内存，与旧 semantic.Service.load 同一做法）。
func (s *SummaryStore) LoadVectors() ([]storedVector, error) {
	rows, err := s.db.Query(`SELECT d.doc_id, d.layer, d.category, v.vec
		FROM doc_vectors v JOIN docs d ON d.doc_id = v.doc_id ORDER BY d.doc_id`)
	if err != nil {
		return nil, fmt.Errorf("summary store load vectors: %w", err)
	}
	defer rows.Close()
	var out []storedVector
	for rows.Next() {
		var sv storedVector
		var blob []byte
		if err := rows.Scan(&sv.DocID, &sv.Layer, &sv.Category, &blob); err != nil {
			return nil, fmt.Errorf("summary store load vectors scan: %w", err)
		}
		v, err := decodeVec(blob)
		if err != nil {
			return nil, fmt.Errorf("summary store load vector %s: %w", sv.DocID, err)
		}
		sv.Vec = v
		out = append(out, sv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("summary store load vectors rows: %w", err)
	}
	return out, nil
}

// ---- 导出物（JSONL/Markdown 只作导出，不强制定时跑）----

// ExportJSONL 把全部简介导出成 JSONL（一行一篇），供外部工具读。
func (s *SummaryStore) ExportJSONL(path string) error {
	docs, err := s.ListDocs()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("summary store export mkdir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("summary store export create: %w", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	for _, d := range docs {
		b, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("summary store export marshal: %w", err)
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return fmt.Errorf("summary store export write: %w", err)
		}
	}
	return nil
}

// ExportMarkdown 把全部简介导出成一个 Markdown 文件（每篇一节），
// 供人阅读与核对。含简介原文，不含向量。
func (s *SummaryStore) ExportMarkdown(path string) error {
	docs, err := s.ListDocs()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("summary store export mkdir: %w", err)
	}
	var b strings.Builder
	b.WriteString("# 简介主库导出\n\n")
	for _, d := range docs {
		fmt.Fprintf(&b, "## %s %s\n\n- 编号：%s\n- 来源：%s\n- 状态：%s\n- 简介：%s\n- 触发词：%s\n\n",
			d.DocID, d.Title, d.DocID, d.SourcePath, d.Status, d.Summary, d.Triggers)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("summary store export write: %w", err)
	}
	return nil
}
