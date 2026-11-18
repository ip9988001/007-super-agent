package memory

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 索引重建的默认值。
const (
	IndexEmbedBatch   = 32   // 每批最多向量化的文档数
	IndexEmbedMaxRunes = 1500 // 单篇待向量化文本上限（字符）
	IndexEmbedTextMax  = 1500
)

// indexDoc 是一篇待索引的记忆文档。
type indexDoc struct {
	Source string // 与 Recall 词法用的源标识一致（wiki 去 .md，l1 留 .log）
	Text   string // 展示文本（≤240 字符，与词法回的一致）
	Embed  string // 待向量化文本（标题+摘要+正文节选）
	Mtime  int64
}

// collectDocs 扫记忆目录，产出全部待索引文档。
func collectDocs(root string) ([]indexDoc, error) {
	var docs []indexDoc
	// Wiki：每页一文，展示文本取 Summary 行（无则取正文前几行）。
	wikiRoot := filepath.Join(root, "wiki")
	_ = filepath.WalkDir(wikiRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = strings.TrimSuffix(rel, ".md")
		summary := ""
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Summary:") {
				summary = strings.TrimSpace(strings.TrimPrefix(line, "Summary:"))
				break
			}
		}
		if summary == "" {
			summary = firstBodyText(string(data))
		}
		title := strings.TrimSuffix(d.Name(), ".md")
		embed := title + "\n" + summary + "\n" + truncateRunes(firstBodyText(string(data)), IndexEmbedMaxRunes)
		info, _ := d.Info()
		docs = append(docs, indexDoc{Source: rel, Text: summary, Embed: embed, Mtime: info.ModTime().UnixNano()})
		return nil
	})
	// L1：每文件一文（7 天日志，文件数少），展示文本取前几行。
	l1Root := filepath.Join(root, "l1")
	entries, _ := os.ReadDir(l1Root)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(l1Root, e.Name()))
		if err != nil {
			continue
		}
		text := firstBodyText(string(data))
		if text == "" {
			continue
		}
		info, _ := e.Info()
		docs = append(docs, indexDoc{
			Source: "l1/" + e.Name(),
			Text:   text,
			Embed:  truncateRunes(string(data), IndexEmbedMaxRunes),
			Mtime:  info.ModTime().UnixNano(),
		})
	}
	// L3 参考：每文件一文，展示文本取 ## 内容 节（无则取正文前几行）。
	refRoot := filepath.Join(root, "reference")
	_ = filepath.WalkDir(refRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = strings.TrimSuffix(rel, ".md")
		text := contentSection(string(data))
		if text == "" {
			text = firstBodyText(string(data))
		}
		if text == "" {
			return nil
		}
		embed := truncateRunes(text, IndexEmbedMaxRunes)
		info, _ := d.Info()
		docs = append(docs, indexDoc{Source: rel, Text: text, Embed: embed, Mtime: info.ModTime().UnixNano()})
		return nil
	})
	return docs, nil
}

// Rebuild 重建向量索引：只对新增/改动过的文档重新向量化（按 mtime），
// 删除已不存在的源，写完重载内存向量表。返回本次向量化的文档数。
func (s *Service) Rebuild(memoryRoot string) (embedded int, err error) {
	docs, err := collectDocs(memoryRoot)
	if err != nil {
		return 0, fmt.Errorf("semantic rebuild collect: %w", err)
	}
	// 读库里已有的 (source, mtime)，只处理新增/改动。
	existing := map[string]int64{}
	rows, err := s.db.Query(`SELECT source, mtime FROM semantic_docs`)
	if err != nil {
		return 0, fmt.Errorf("semantic rebuild read: %w", err)
	}
	for rows.Next() {
		var src string
		var mt int64
		if err := rows.Scan(&src, &mt); err == nil {
			existing[src] = mt
		}
	}
	rows.Close()
	// 待向量化的文档。
	var todo []indexDoc
	for _, d := range docs {
		if mt, ok := existing[d.Source]; !ok || mt != d.Mtime {
			todo = append(todo, d)
		}
	}
	// 分批向量化并写入。
	for i := 0; i < len(todo); i += IndexEmbedBatch {
		j := i + IndexEmbedBatch
		if j > len(todo) {
			j = len(todo)
		}
		batch := todo[i:j]
		texts := make([]string, len(batch))
		for k, d := range batch {
			texts[k] = d.Embed
		}
		vecs, err := s.embed(texts)
		if err != nil {
			return embedded, fmt.Errorf("semantic rebuild embed batch %d-%d: %w", i, j, err)
		}
		for k, d := range batch {
			if len(vecs[k]) == 0 {
				log.Printf("semantic rebuild: %s 向量为空，跳过", d.Source)
				continue
			}
			if _, err := s.db.Exec(
				`INSERT INTO semantic_docs(source, text, mtime, vec) VALUES(?,?,?,?)
				 ON CONFLICT(source) DO UPDATE SET text=excluded.text, mtime=excluded.mtime, vec=excluded.vec`,
				d.Source, truncateRunes(d.Text, MaxResultRunes), d.Mtime, encodeVec(vecs[k])); err != nil {
				return embedded, fmt.Errorf("semantic rebuild upsert %s: %w", d.Source, err)
			}
			embedded++
		}
		log.Printf("semantic rebuild: 已向量化 %d/%d 篇", embedded, len(todo))
	}
	// 删除已不存在的源。
	alive := map[string]bool{}
	for _, d := range docs {
		alive[d.Source] = true
	}
	for src := range existing {
		if !alive[src] {
			if _, err := s.db.Exec(`DELETE FROM semantic_docs WHERE source=?`, src); err != nil {
				return embedded, fmt.Errorf("semantic rebuild delete %s: %w", src, err)
			}
			log.Printf("semantic rebuild: 删除失效源 %s", src)
		}
	}
	// 写 meta 并重载内存。
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(
		`INSERT INTO semantic_meta(key, value) VALUES('model',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, s.model); err != nil {
		return embedded, fmt.Errorf("semantic rebuild meta model: %w", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO semantic_meta(key, value) VALUES('built_at',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, now); err != nil {
		return embedded, fmt.Errorf("semantic rebuild meta built_at: %w", err)
	}
	if err := s.load(); err != nil {
		return embedded, fmt.Errorf("semantic rebuild reload: %w", err)
	}
	return embedded, nil
}
