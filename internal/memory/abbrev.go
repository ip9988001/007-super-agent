package memory

import (
	"strings"
	"unicode"
)

// 缩写映射表（v2.1 §7）：只在查询扩展阶段单向展开——输入含缩写
// 时补上全称一起查（CF → CF + Cloudflare），绝不反向（输入
// Cloudflare 不补 CF）。软上限 50 条，现 4 条，每条的全称都已
// 在 wiki/l3 记忆库里实测存在（括号内是含全称的文件数，
// 2026-11-24 在 /tmp/mem-b-line-b 上 grep 核实）：
//   - CF → Cloudflare（95 篇）：口语问法 C01 的实测用例；
//   - DNS → 域名解析（2 篇）；URL → 网址（1 篇）；
//     AI → 人工智能（1 篇）。
//
// 其余常见缩写（OTP/TOTP/GFW/CDN/VPN/SSH/SIM 等）的中文全称
// 在库里一篇都不存在，展开只加噪音，故不收录；新缩写须先核实
// 全称在库里存在再加表，禁止凭感觉加。
const maxAbbreviations = 50

// Abbreviations 是缩写 → 全称表（键按原样大小写比对，
// ASCII 缩写比对时不区分大小写）。
var Abbreviations = map[string][]string{
	"CF":  {"Cloudflare"},
	"DNS": {"域名解析"},
	"URL": {"网址"},
	"AI":  {"人工智能"},
}

// expandAbbreviations 对查询做单向缩写展开：缩写出现在词边界上
// （前一个字符不是 ASCII 字母/数字、后一个字符也不是 ASCII
// 字母/数字，如“CF注册”里的 CF、“CF ”里的 CF；“CFO”里的 CF
// 不算）时，把全称追加到查询末尾一起查。原查询一字不动，
// 只追加。返回展开后的查询与追加的全称列表（无展开时后者为空）。
func expandAbbreviations(query string) (string, []string) {
	added := []string{}
	seen := map[string]bool{}
	lower := strings.ToLower(query)
	for abbr, fulls := range Abbreviations {
		if len(Abbreviations) > maxAbbreviations {
			break
		}
		needle := strings.ToLower(abbr)
		if !hasAbbreviationToken(lower, needle) {
			continue
		}
		for _, f := range fulls {
			if !seen[f] {
				seen[f] = true
				added = append(added, f)
			}
		}
	}
	if len(added) == 0 {
		return query, nil
	}
	return query + " " + strings.Join(added, " "), added
}

// hasAbbreviationToken 报告小写化后的查询里是否含词边界上的缩写。
func hasAbbreviationToken(lowerQuery, needle string) bool {
	for i := 0; i+len(needle) <= len(lowerQuery); i++ {
		if lowerQuery[i:i+len(needle)] != needle {
			continue
		}
		if i > 0 {
			if c := rune(lowerQuery[i-1]); c < 128 && (unicode.IsLetter(c) || unicode.IsDigit(c)) {
				continue
			}
		}
		j := i + len(needle)
		if j < len(lowerQuery) {
			if c := rune(lowerQuery[j]); c < 128 && (unicode.IsLetter(c) || unicode.IsDigit(c)) {
				continue
			}
		}
		return true
	}
	return false
}
