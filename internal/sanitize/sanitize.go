// Package sanitize 是 v2.1 第六节的脱敏中间件：无状态，正则规则 +
// 高熵长串检测，命中则替换为不可逆掩码 [REDACTED]。
//
// 挂载节点（用户 2026-11-24 09:12 纠正后的口径，覆盖 v2.1 §6 旧文；
// 密钥必须保持可用，007 要用密钥调 API 才能干活）：
//  1. 写入口 WriteWiki 落盘前——已取消（09:12 明令），
//     WriteWiki 不再掩码，落盘原文保持可用；
//  2. 简介入库前（简介生成输入掩码、简介输出含密钥打回，
//     internal/summary 的 CheckAll / SanitizeForSummary）；
//  3. 召回账本写查询 + 结果前（internal/ledger.RecordRecall）；
//  4. 卡片 / 全文交付给模型前——已取消（09:12 明令），
//     memorySummary / dispatchMemorySummary / front.doRecall
//     一律不再掩码，模型照常看、照常用密钥。
//  5. 出站硬扫描（线 B 修复轮第 3 棒；Gemini 审阅第三节缺口一 /
//     第十节 A 清单第 3 项）：最终 reply.md 落盘与回传网关前，
//     ScrubOutbound 做一次强制掩码，脱敏后仍有残留则整封拦截
//     （BlockedReply）。只拦最终给用户看的报告；模型干活调工具时
//     照常用明文。
//
// 巡检原地改写（RedactFile 自动重写）也已取消：函数保留只供
// 人工调用，绝不自动改生产记忆文件。
//
// 测试一律用合成假密钥，绝不用真实密钥值做测试。
package sanitize

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Redacted 是不可逆掩码：替换掉全部命中值，不保留任何原文片段。
const Redacted = "[REDACTED]"

// kvKeys 是“键名 = 值”形态的凭证键名（不区分大小写），命中时只遮值、留键名。
var kvKeys = strings.Join([]string{
	`api[_-]?key`, `api[_-]?secret`, `app[_-]?secret`, `client[_-]?secret`,
	`access[_-]?token`, `auth[_-]?token`, `refresh[_-]?token`, `bot[_-]?token`,
	`secret`, `password`, `passwd`, `pwd`, `token`,
	`global[_-]?api[_-]?key`, `private[_-]?key`,
}, "|")

// patterns 是按顺序应用的正则替换规则：先整块、再键值、再裸 token。
// 每条的替换模板里 $1 保留无害前缀（键名 / Bearer / scheme），值一律遮掉。
var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM 私钥整块（含头尾行与中间全部内容）。
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), Redacted},
	// 键名 = 值：api_key=... / password: "..." / token=... 等，值遮掉、键名保留。
	// 值字符集排除空白、引号、反引号、[（掩码 "[REDACTED]" 本身不再匹配，
	// 否则脱敏后复查永远报残留）、中英文标点与换行，防值贪婪吞掉后文。
	{regexp.MustCompile(`(?i)((?:` + kvKeys + `)\s*[:=]\s*["']?)[^\s"'` + "`" + `\[，；：、。\n,;}]+`), `${1}` + Redacted},
	// Bearer 令牌。
	{regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._\-~+/=]{10,}`), `${1}` + Redacted},
	// URL 里嵌的账密：scheme://user:pass@host → scheme://[REDACTED]@host。
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`), `${1}` + Redacted + `@`},
	// 常见服务商 token 前缀（合成假值同样命中，测试不用真值）。
	{regexp.MustCompile(`\b(?:sk|pk)_(?:live|test)_[A-Za-z0-9]{8,}\b`), Redacted},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9]{10,}\b`), Redacted},
	// sk-proj- / sk-svcacct-：OpenAI 项目密钥与服务账号密钥含第二段
	// 前缀，上面那条 sk- 规则在第二个 "-" 处断掉、漏网，单列一条。
	{regexp.MustCompile(`\bsk-(?:proj|svcacct)-[A-Za-z0-9]{10,}\b`), Redacted},
	{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9\-_]{10,}\b`), Redacted},
	{regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b`), Redacted},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), Redacted},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`), Redacted},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), Redacted},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), Redacted},
	// 长十六进制串（≥32 位）：Cloudflare Global API Key 之类纯 hex 凭证的
	// 香农熵上限只有 4.0，走不到高熵规则，须单列一条。误伤 git SHA 可接受
	// （召回文本里遮掉一个 SHA 不影响任务，账本里更不该留）。
	{regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`), Redacted},
}

// highEntropyToken 找长度 ≥20 的连续无空白串（字母数字及 _ - + / = .），
// 逐个算香农熵，>4.5 的按凭证串处理。
var highEntropyToken = regexp.MustCompile(`[A-Za-z0-9_\-+/=.]{20,}`)

// entropyThreshold 是 v2.1 第六节定死的阈值。
const entropyThreshold = 4.5

// shannonEntropy 算字符串的香农熵（按字节分布，单位 bit）。
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	freq := make(map[byte]int, len(s))
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	ent := 0.0
	n := float64(len(s))
	for _, c := range freq {
		p := float64(c) / n
		ent -= p * math.Log2(p)
	}
	return ent
}

// sanitizeCount 返回脱敏后文本与替换次数（内部用，Sanitize 只取文本）。
func sanitizeCount(text string) (string, int) {
	n := 0
	out := text
	for _, p := range patterns {
		if p.re.MatchString(out) {
			matches := p.re.FindAllStringIndex(out, -1)
			n += len(matches)
			out = p.re.ReplaceAllString(out, p.repl)
		}
	}
	// 高熵长串：只处理上一步没遮掉的残留串。
	out = highEntropyToken.ReplaceAllStringFunc(out, func(tok string) string {
		if shannonEntropy(tok) > entropyThreshold {
			n++
			return Redacted
		}
		return tok
	})
	return out, n
}

// Sanitize 对文本脱敏：命中凭证串则替换为不可逆掩码。无命中时原样返回。
// 幂等：Sanitize(Sanitize(x)) == Sanitize(x)。
func Sanitize(text string) string {
	out, _ := sanitizeCount(text)
	return out
}

// SanitizeCount 返回脱敏后文本与替换次数（供巡检统计与测试断言用）。
func SanitizeCount(text string) (string, int) {
	return sanitizeCount(text)
}

// ContainsSecret 报告文本里是否含有可脱敏的凭证串（只返回布尔值，
// 绝不返回命中内容；巡检只计数、不输出值）。
func ContainsSecret(text string) bool {
	_, n := sanitizeCount(text)
	return n > 0
}

// SanitizeForSummary 是节点 2（简介入库前）的调用点：与 Sanitize 同一
// 规则，只是名字点明用途。第二阶段简介库建好后直接调它入库前过一遍。
// 本阶段没有简介库、无调用点；函数与单测已就绪，待第二阶段接入。
func SanitizeForSummary(summary string) string {
	return Sanitize(summary)
}

// BlockedReply 是出站硬拦截后的兜底回信：脱敏后复查仍有残留时，
// 宁可整封拦下也不让原文出站。固定文本，不含任何任务内容。
const BlockedReply = "任务已执行，但回信内容疑似含有密钥信息，已按安全规则拦截，未能完整展示。请重新下发任务，任务描述中不要包含密钥原文。\n"

// ScrubOutbound 对即将出站给用户看的最终回信做硬扫描（线 B 修复轮
// 第 3 棒；Gemini 审阅第三节缺口一、第十节 A 清单第 3 项）：
//  1. 先 Sanitize 掩码全部命中（高危固定模式 sk- / AKIA / ghp_ /
//     Bearer / BEGIN PRIVATE KEY 等，复用现有 patterns）；
//  2. 再复查：脱敏后仍有残留（理论上不应发生，Sanitize 幂等）则
//     整封拦截，返回 BlockedReply 与 blocked=true，调用方必须落盘
//     该兜底文本、绝不落盘原文。
//
// 只用在最终 reply.md 落盘 / 回传网关前。模型干活调工具时一律不调
// （用户 2026-11-24 09:12 原则：密钥必须保持可用，只拦最终报告）。
// 第 4 节点（交付给模型前）掩码、巡检原地改写、WriteWiki 落盘前掩码
// 仍保持取消，不恢复。
// 返回（出站文本，掩码处数，是否整封拦截）；只记数量，绝不回传命中值。
func ScrubOutbound(text string) (clean string, redacted int, blocked bool) {
	clean, redacted = sanitizeCount(text)
	if ContainsSecret(clean) {
		return BlockedReply, redacted, true
	}
	return clean, redacted, false
}

// ScanReport 是巡检扫描的只计数报告：只有数量，没有任何原文或命中值。
type ScanReport struct {
	FilesScanned     int            // 扫过的文件数
	FilesWithSecrets int            // 含凭证串的文件数
	TotalHits        int            // 替换次数合计
	ByDir            map[string]int // 目录 -> 含凭证文件数
}

// ScanDir 只读巡检扫描：递归扫 root 下的文本文件（.md/.log/.txt），
// 统计含凭证串的文件数与命中数，绝不输出任何命中值、不改任何文件。
// vectors.db 等二进制文件跳过。root 不存在返回错误。
func ScanDir(root string) (ScanReport, error) {
	rep := ScanReport{ByDir: map[string]int{}}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".log" && ext != ".txt" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rep.FilesScanned++
		if _, n := sanitizeCount(string(data)); n > 0 {
			rep.FilesWithSecrets++
			rep.TotalHits += n
			rel, _ := filepath.Rel(root, path)
			rep.ByDir[filepath.Dir(rel)]++
		}
		return nil
	})
	if err != nil {
		return rep, fmt.Errorf("sanitize scan %s: %w", root, err)
	}
	return rep, nil
}

// RedactFile 把单个文件就地脱敏重写（巡检发现绕过入口的文件时调用）：
// 先脱敏、确认 0 敏感命中后才落盘；无命中时不碰文件。返回是否改动过。
// 注意：用户 2026-11-24 09:12 已取消巡检原地改写——本函数只供人工
// 调用，绝不自动改生产记忆文件；何时对生产记忆启用自动重写，
// 须用户另行决定。
func RedactFile(path string) (changed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("sanitize redact read %s: %w", path, err)
	}
	out, n := sanitizeCount(string(data))
	if n == 0 {
		return false, nil
	}
	if ContainsSecret(out) {
		return false, fmt.Errorf("sanitize redact %s: 脱敏后仍有残留，拒绝落盘", path)
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return false, fmt.Errorf("sanitize redact write %s: %w", path, err)
	}
	return true, nil
}
