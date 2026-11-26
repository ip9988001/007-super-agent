package summary

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Embedder 是简介向量接口：规则⑦同目录查重用。生产实现走服务器
// Ollama qwen3-embedding:0.6b（127.0.0.1:11434），单测用假实现。
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// CollisionThreshold 是规则⑦的打回阈值（v2.1 §6）：新简介向量与
// 同子目录已有简介的余弦相似度 >0.88 打回。
const CollisionThreshold = 0.88

// CosineSimilarity 算两向量的余弦相似度（长度不等或零向量返回 0）。
func CosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// CollisionChecker 持同子目录已入库简介的向量，供规则⑦查重。
// key 是源文件所在子目录（如 wiki/entities），value 是该目录下
// 已接受简介的（doc_id，简介正文，向量）。
type CollisionChecker struct {
	embedder Embedder
	byDir    map[string][]acceptedSummary
}

type acceptedSummary struct {
	DocID   string
	Summary string
	Vector  []float32
}

// NewCollisionChecker 建查重器（embedder 为 nil 时规则⑦全部跳过并记明）。
func NewCollisionChecker(embedder Embedder) *CollisionChecker {
	return &CollisionChecker{embedder: embedder, byDir: map[string][]acceptedSummary{}}
}

// CheckCollision 是规则⑦：新简介向量与同子目录已有简介余弦相似度
// >0.88 打回，并在原因里附上撞车简介（供重写提示用）。
// embedder 不可用时返回 Skipped=true 的结果，不阻塞入库，须在汇报单列。
func (c *CollisionChecker) CheckCollision(ctx context.Context, dir, docID, summaryText string) CheckResult {
	if c.embedder == nil {
		return CheckResult{Rule: 7, Name: "同目录查重", Pass: true, Skipped: true, Reason: "未配向量服务，规则⑦跳过"}
	}
	vec, err := c.embedder.Embed(ctx, summaryText)
	if err != nil {
		return CheckResult{Rule: 7, Name: "同目录查重", Pass: true, Skipped: true, Reason: fmt.Sprintf("向量服务调用失败，规则⑦跳过：%v", err)}
	}
	for _, acc := range c.byDir[dir] {
		if acc.DocID == docID {
			continue
		}
		if sim := CosineSimilarity(vec, acc.Vector); sim > CollisionThreshold {
			return fail(7, "同目录查重", fmt.Sprintf("与同目录 %s 的简介余弦相似度 %.2f > 0.88，撞车简介：%q", acc.DocID, sim, acc.Summary))
		}
	}
	return pass(7, "同目录查重", "与同目录已有简介无撞车")
}

// Accept 把通过全部质检的简介记入查重器（供后续同目录文档查重）。
// 向量调用失败时只记文本不记向量，该条不参与后续查重，并在原因里说明。
func (c *CollisionChecker) Accept(ctx context.Context, dir, docID, summaryText string) error {
	if c.embedder == nil {
		c.byDir[dir] = append(c.byDir[dir], acceptedSummary{DocID: docID, Summary: summaryText})
		return nil
	}
	vec, err := c.embedder.Embed(ctx, summaryText)
	if err != nil {
		c.byDir[dir] = append(c.byDir[dir], acceptedSummary{DocID: docID, Summary: summaryText})
		return fmt.Errorf("summary collision accept %s: 向量失败，仅记文本：%w", docID, err)
	}
	c.byDir[dir] = append(c.byDir[dir], acceptedSummary{DocID: docID, Summary: summaryText, Vector: vec})
	return nil
}

// ---- Ollama 向量实现 ----

// OllamaEmbedder 调本地 Ollama /api/embeddings（模型 qwen3-embedding:0.6b）。
type OllamaEmbedder struct {
	BaseURL string // 如 http://127.0.0.1:11434
	Model   string // qwen3-embedding:0.6b
	HTTP    *http.Client
}

// NewOllamaEmbedder 建默认 Ollama 向量器。
func NewOllamaEmbedder() *OllamaEmbedder {
	return &OllamaEmbedder{
		BaseURL: "http://127.0.0.1:11434",
		Model:   "qwen3-embedding:0.6b",
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embed 取一段文本的向量（只返回向量，不记任何文本进日志）。
func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(ollamaEmbedRequest{Model: o.Model, Prompt: text})
	if err != nil {
		return nil, fmt.Errorf("ollama embed marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := o.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed status %d", resp.StatusCode)
	}
	var out ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama embed decode: %w", err)
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("ollama embed: 空向量")
	}
	return out.Embedding, nil
}
