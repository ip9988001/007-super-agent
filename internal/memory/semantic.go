// Package memory 的语义检索：本地 Ollama embedding（qwen3-embedding:0.6b）+
// SQLite 向量库。词法是必有的底，语义是注入的增强：Recall 时两路合并，
// 同源取最高分。Ollama 挂了只降级回词法，不让 Recall 整体失败。
package memory

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// 语义检索的默认值。
const (
	SemanticDefaultURL   = "http://127.0.0.1:11434"
	SemanticDefaultModel = "qwen3-embedding:0.6b"
	SemanticTopK         = 5    // 语义一路最多回 5 条，再与词法合并取前 3
	SemanticMinSim       = 0.30 // 余弦相似度低于此值不回
	SemanticScoreScale   = 100  // 相似度转整数分：Score = int(sim*100)，0–100。语义是更准的
	// 相关性信号，故意让它能压过词法命中次数（常见 10–40）：换说法
	// 的查询（如“绕过机器人验证”）词法会错配高频词（机器人→企业微信
	// 机器人），语义才能把真正相关的 camoufox 那条顶上来。两路同源
	// 仍取最高分（memory.Recall 的合并规则不变）。
	SemanticEmbedTimeout = 120 * time.Second
)

// QueryInstruction 是查问句的指令前缀：Qwen3-Embedding 是指令感知模型，
// 检索任务的问句按模型卡的用法带指令，文档不带（非对称检索是设计用法）。
const QueryInstruction = "Instruct: Given a question, retrieve relevant passages that answer the question\nQuery: "

// EmbedFunc 是向量化函数：一次可多条，返回与输入等长的向量。
// 生产用 Ollama，测试用桩。
type EmbedFunc func(texts []string) ([][]float32, error)

// semanticDoc 是内存中的一条向量文档。
type semanticDoc struct {
	Source string
	Text   string // 展示文本（≤240 字符，与词法回的一致）
	Vec    []float32
}

// Service 是语义检索服务：Ollama embedding + SQLite 向量库 + 内存向量表。
type Service struct {
	dbPath string
	db     *sql.DB
	url    string
	model  string
	dim    int
	embed  EmbedFunc

	mu   sync.RWMutex
	docs []semanticDoc
}

// OpenSemantic 打开（不存在则创建）向量库并把全部向量读进内存。
// Ollama 不通时只记日志、不报错，Search 会降级回空结果（词法兜底）。
func OpenSemantic(dbPath, ollamaURL, model string) (*Service, error) {
	if ollamaURL == "" {
		ollamaURL = SemanticDefaultURL
	}
	if model == "" {
		model = SemanticDefaultModel
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("semantic open db: %w", err)
	}
	// 单连接串行写 + WAL + synchronous=NORMAL + busy_timeout=5000：
	// 与简介主库、账本同一口径，防与巡检/前台写库撞上 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("semantic pragma WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("semantic pragma synchronous: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("semantic pragma busy_timeout: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS semantic_docs (
		source TEXT PRIMARY KEY, text TEXT NOT NULL, mtime INTEGER NOT NULL, vec BLOB NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("semantic create docs: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS semantic_meta (
		key TEXT PRIMARY KEY, value TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("semantic create meta: %w", err)
	}
	s := &Service{dbPath: dbPath, db: db, url: strings.TrimSuffix(ollamaURL, "/"), model: model}
	s.embed = s.ollamaEmbed
	if err := s.load(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Model 返回向量模型名。
func (s *Service) Model() string { return s.model }

// Count 返回内存中的向量文档数。
func (s *Service) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.docs)
}

// Dim 返回向量维度（库为空时为 0）。
func (s *Service) Dim() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dim
}

// Close 关闭向量库。
func (s *Service) Close() error { return s.db.Close() }

// load 把库里的全部向量读进内存。
func (s *Service) load() error {
	rows, err := s.db.Query(`SELECT source, text, vec FROM semantic_docs ORDER BY source`)
	if err != nil {
		return fmt.Errorf("semantic load: %w", err)
	}
	defer rows.Close()
	var docs []semanticDoc
	dim := 0
	for rows.Next() {
		var d semanticDoc
		var blob []byte
		if err := rows.Scan(&d.Source, &d.Text, &blob); err != nil {
			return fmt.Errorf("semantic load scan: %w", err)
		}
		v, err := decodeVec(blob)
		if err != nil {
			return fmt.Errorf("semantic load vec %s: %w", d.Source, err)
		}
		if dim == 0 {
			dim = len(v)
		} else if len(v) != dim {
			log.Printf("semantic load: %s 维度 %d 与库 %d 不一致，跳过", d.Source, len(v), dim)
			continue
		}
		d.Vec = v
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("semantic load rows: %w", err)
	}
	s.mu.Lock()
	s.docs = docs
	s.dim = dim
	s.mu.Unlock()
	return nil
}

// OllamaEmbedFunc 返回一个直调 Ollama /api/embed 的 EmbedFunc
// （线 B 第二阶段第 3 棒：两阶段召回的问句向量化用，不经过
// Service 的向量库；Ollama 不通时调用方降级，不报错）。
// url/model 为空时取默认值（本地 127.0.0.1:11434，
// qwen3-embedding:0.6b）。
func OllamaEmbedFunc(url, model string) EmbedFunc {
	if url == "" {
		url = SemanticDefaultURL
	}
	if model == "" {
		model = SemanticDefaultModel
	}
	s := &Service{url: strings.TrimSuffix(url, "/"), model: model}
	return s.ollamaEmbed
}

// ollamaEmbed 调 Ollama /api/embed（支持批量），返回与输入等长的向量。
func (s *Service) ollamaEmbed(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{"model": s.model, "input": texts})
	if err != nil {
		return nil, fmt.Errorf("semantic embed marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, s.url+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("semantic embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: SemanticEmbedTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("semantic embed ollama %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("semantic embed read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("semantic embed ollama HTTP %d: %s", resp.StatusCode, truncateRunes(string(data), 200))
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("semantic embed decode: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("semantic embed: 要 %d 条回 %d 条", len(texts), len(out.Embeddings))
	}
	return out.Embeddings, nil
}

// Search 是 SemanticSearchFunc 的实现：问句向量化后算余弦相似度，
// 取前 5 条（相似度 ≥0.30），Score = int(sim*10)。Ollama 出错时记日志、
// 回空结果（不报错），Recall 降级只用词法。
func (s *Service) Search(query string) ([]Result, error) {
	s.mu.RLock()
	docs := s.docs
	s.mu.RUnlock()
	if len(docs) == 0 {
		return nil, nil
	}
	vecs, err := s.embed([]string{QueryInstruction + query})
	if err != nil {
		log.Printf("semantic search embed 失败（降级词法）: %v", err)
		return nil, nil
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, nil
	}
	q := normalize(vecs[0])
	type hit struct {
		d   semanticDoc
		sim float64
	}
	hits := make([]hit, 0, len(docs))
	for _, d := range docs {
		if len(d.Vec) != len(q) {
			continue
		}
		sim := cosine(q, normalize(d.Vec))
		if sim >= SemanticMinSim {
			hits = append(hits, hit{d: d, sim: sim})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].sim > hits[j].sim })
	if len(hits) > SemanticTopK {
		hits = hits[:SemanticTopK]
	}
	out := make([]Result, 0, len(hits))
	for _, h := range hits {
		out = append(out, Result{
			Source: h.d.Source,
			Text:   truncateRunes(h.d.Text, MaxResultRunes),
			Score:  int(h.sim * SemanticScoreScale),
		})
	}
	return out, nil
}

// normalize 返回单位向量（已是单位向量时原样返回）。
func normalize(v []float32) []float32 {
	norm := float32(0)
	for _, x := range v {
		norm += x * x
	}
	norm = float32(math.Sqrt(float64(norm)))
	if norm == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

// cosine 算两个单位向量的余弦相似度（即点积）。
func cosine(a, b []float32) float64 {
	sum := float64(0)
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

// encodeVec 把向量存成小端 float32。
func encodeVec(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return buf
}

// decodeVec 从小端 float32 读回向量。
func decodeVec(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("向量字节数 %d 不是 4 的倍数", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}
