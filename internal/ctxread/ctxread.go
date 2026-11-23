// Package ctxread —— 007 执行端的压缩读（必经之路）。v3（按 Gemini v2 复审修过）。
//
// 抄的是三号上 pi-lean-ctx 的核心思路，只抄“读文件分档压缩”这一块，
// 不是整个包——pi 的接线、MCP、会话管理那些 007 用不上。
//
// 读文件时先按文件类型和大小分档：
//   全读：小文件（代码 <8KB；文档/数据文件 ≤24KB；非代码文件 ≤24KB）
//   结构图（map）：中等代码文件（8KB–96KB）、文档/数据文件 >24KB、非代码文件 >24KB
//   签名（signatures）：大代码文件（≥96KB），只给函数/类型签名，不给函数体
// 模型真要看哪几行，再用行号窗口去取。
//
// 这样大文件永远不会整篇进模型的上下文，省 token 也省上下文额度。
// 这条路是必经之路：执行端读文件只走这里，不留直接读全文的口子。
// 想看全文，只能指定行号窗口（ReadLines），一次最多 2000 行、24KB。
//
// v3 改动（Gemini v2 复审 2026-11-23，3 处必须改 + 3 处建议改，全修；
// 复审里 indentPrefix 那处 Gemini 看错了——代码本来就是制表符换两格，不用改）：
//   必须改 1：capBytes 改用 utf8.RuneStart，最多回退 3 字节（严格 O(1)）；之前用 utf8.ValidString 循环回退，
//     文件前部混进 1 个非 UTF-8 字节就会一路退到那字节之前，O(N^2) 慢扫还把后面正常内容全砍掉。
//   必须改 2：truncateText 只管按行截，字节截断统一交给 Read 出口的 capBytes 干一次；之前先截字节拼提示、
//     出口又切一次，会把提示语切碎。
//   必须改 3：genericSignatures 每行带行号（%d: ...），并过单行长度上限；之前没打行号，模型没法接 ReadLines。
//   建议改 1：readFileSafe 与 ChooseMode 加 !st.Mode().IsRegular()，防 /dev/zero、命名管道绕过 10MB 检查导致 OOM 或卡死。
//   建议改 2：NonCodeMapMinBytes 从 48KB 降到 24KB（对齐 24KB 返回上限）；codeExtensions 补 .h/.hpp；
//     ReadLines 的 end <= 0 默认读到末尾（防模型传 end=0 报错）。
//   必须改（装机步骤，不在本文件）：executor.go 的 bash 返回层加统一硬截断兜底（超 16–24KB 落盘给路径指针 + 提示用 ctxread），
//     封死 cat/python 绕过。见文档第五节。
package ctxread

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// 上限（v2 拆成两档，见 Gemini 审阅重点 4）。
const (
	// MaxReadFileBytes 读盘上限：单个文件超过这个数，拒绝全量读进内存（防 OOM）。
	MaxReadFileBytes = 10 * 1024 * 1024
	// DefaultMaxBytes 返回给模型的硬上限：约 6000 token，对齐 E-12 R2 预算。
	DefaultMaxBytes = 24 * 1024
	// CodeFullReadMaxBytes 代码文件小于这个数，全读。
	CodeFullReadMaxBytes = 8 * 1024
	// CodeSignaturesMinBytes 代码文件达到这个数，只给签名。
	CodeSignaturesMinBytes = 96 * 1024
	// NonCodeMapMinBytes 非代码、非文档类文件超过这个数，给结构图（v3：从 48KB 降到 24KB，对齐 24KB 返回上限）。
	NonCodeMapMinBytes = 24 * 1024
	// DocMapMinBytes 文档/数据文件超过这个数，给结构图（不再无上限全读）。
	DocMapMinBytes = 24 * 1024
	// MaxLineBytes 单行最大字节数（防 5MB 单行文件撑爆上下文）。
	MaxLineBytes = 500
	// DefaultMaxLines 单次返回最多行。
	DefaultMaxLines = 2000
)

// 代码文件后缀（抄 lean-ctx 的 CODE_EXTENSIONS）。
var codeExtensions = map[string]bool{
	".rs": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".php": true, ".py": true, ".go": true, ".java": true, ".c": true,
	".h": true, ".hpp": true, ".cc": true, ".cpp": true, ".cxx": true, ".cs": true, ".kt": true,
	".swift": true, ".rb": true, ".vue": true, ".svelte": true,
	".astro": true, ".html": true, ".css": true, ".scss": true,
	".sass": true, ".less": true, ".lua": true, ".zig": true,
	".nim": true, ".ex": true, ".exs": true, ".erl": true,
	".hs": true, ".ml": true, ".mli": true, ".r": true, ".jl": true,
	".dart": true, ".scala": true, ".groovy": true, ".pl": true,
	".pm": true, ".sh": true, ".bash": true, ".zsh": true,
	".fish": true, ".ps1": true, ".bat": true, ".cmd": true,
	".sql": true, ".graphql": true, ".gql": true, ".proto": true,
	".thrift": true, ".tf": true, ".hcl": true, ".nix": true,
	".dhall": true,
}

// 全读文件后缀（抄 lean-ctx 的 FULL_READ_EXTENSIONS）：文档和数据文件。
// v2：不再无上限全读，超过 DocMapMinBytes（24KB）走 ModeMap。
var fullReadExtensions = map[string]bool{
	".md": true, ".txt": true, ".json": true, ".json5": true,
	".yaml": true, ".yml": true, ".toml": true, ".env": true,
	".ini": true, ".xml": true, ".lock": true,
}

// 包级预编译正则（v2：yaml/md 的正则提到包级，避免每次调用重复编译）。
var (
	reYAMLKey  = regexp.MustCompile(`^([A-Za-z0-9_.-]+):`)
	reMDHead   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	rePyDef    = regexp.MustCompile(`(?m)^\s*(async\s+def|def|class)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	reJSFunc   = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	reJSClass  = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	reJSConst  = regexp.MustCompile(`(?m)^\s*(?:export\s+)?const\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s*)?\(?[^)]*\)?\s*=>`)
	reJSMethod = regexp.MustCompile(`(?m)^\s*(?:public\s+|private\s+|protected\s+|static\s+|async\s+)*([A-Za-z_$][A-Za-z0-9_$]*)\s*\([^;]*\)\s*\{`)
	// reCFunc 只给 C/Java/C# 用，且排除关键字、要求行尾带 {，防 return foo(x) 误伤。
	reCFunc  = regexp.MustCompile(`(?m)^\s*(?:[A-Za-z_][A-Za-z0-9_*\s]+?)\s+([A-Za-z_][A-Za-z0-9_]*)\s*\([^;]*\)\s*\{`)
	reShFunc = regexp.MustCompile(`(?m)^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(\)\s*\{`)
	reSQLObj = regexp.MustCompile(`(?im)^\s*CREATE\s+(?:OR\s+REPLACE\s+)?(?:TABLE|VIEW|FUNCTION|PROCEDURE|INDEX)\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_.]*)`)
)

// 排除的关键字（行首是这些词的不是函数/方法定义）。
var cKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true,
	"return": true, "catch": true, "sizeof": true, "else": true,
	"do": true, "case": true, "await": true,
}

// outlineRule 是一条结构图正则：种类 + 正则。
type outlineRule struct {
	kind string
	re   *regexp.Regexp
}

// Mode 是读文件的方式。
type Mode string

const (
	// ModeAuto 自动分档（默认）：按文件类型和大小选 full/map/signatures。
	ModeAuto Mode = "auto"
	// ModeFull 全文。
	ModeFull Mode = "full"
	// ModeFullCompact 全文但压掉多余空行和行尾空格。
	ModeFullCompact Mode = "full-compact"
	// ModeMap 只给结构图。
	ModeMap Mode = "map"
	// ModeSignatures 只给签名。
	ModeSignatures Mode = "signatures"
	// ModeRaw 原样字节（只在必须看绝对原文时用）。
	ModeRaw Mode = "raw"
)

// Result 是一次读文件的结果。
type Result struct {
	Mode       Mode   // 实际用的方式
	Text       string // 返回给模型的文本（末尾带压缩脚注）
	Lines      int    // 文件总行数
	Truncated  bool   // 是否被截断过
	OrigTokens int    // 原文估算 token 数
	OutTokens  int    // 返回文本估算 token 数
}

// estimateTokens 估算 token 数（英文和代码约 4 个字符 1 个 token；中文约 1.3 个字符 1 个 token，
// 按 DeepSeek 约 0.6–0.75 token/字估，v2 系数与注释对齐）。只是估算，用来写脚注和做预算，不做精确计费。
func estimateTokens(s string) int {
	n := 0
	for _, r := range s {
		if r < 128 {
			n++
		} else {
			n += 3
		}
	}
	if n == 0 {
		return 0
	}
	t := (n + 3) / 4
	if t < 1 {
		return 1
	}
	return t
}

// footer 在返回文本末尾加一行压缩脚注，和 lean-ctx 的格式一致，方便对账。
func footer(orig, out int) string {
	if orig <= 0 {
		return ""
	}
	pct := 0
	if orig > out {
		pct = (orig - out) * 100 / orig
	}
	return fmt.Sprintf("\n\nCompressed %d → %d tokens (-%d%%)", orig, out, pct)
}

// capBytes 按字节上限截断文本，只看截断点是不是多字节字符的中间字节，最多回退 3 个字节（严格 O(1)）。
// v3 修：之前用 utf8.ValidString(s[:cut]) 循环回退，文件前部混进 1 个非 UTF-8 字节就会一路退到那字节之前，
// 既 O(N^2) 慢扫又把后面的正常内容全砍掉。现在只在截断点附近看，不受前文损坏字节影响。
func capBytes(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && cut > maxBytes-utf8.UTFMax && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// readFileSafe 安全读文件：判目录、判大小（防 OOM）、判二进制。
// 超过 MaxReadFileBytes 拒绝全量读，报错提示用脚本处理；
// 二进制文件不吐乱码，只返回元信息。
func readFileSafe(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("ctxread: 文件不存在或不可读 %s: %w", path, err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("ctxread: %s 是目录，不是文件", path)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("ctxread: %s 不是普通文件（设备文件或管道），拒绝读取", path)
	}
	if st.Size() > MaxReadFileBytes {
		return nil, fmt.Errorf("ctxread: 文件太大（%d 字节，超过 %d 上限），请用脚本统计或分段处理，不要整篇读", st.Size(), MaxReadFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ctxread: 读文件失败 %s: %w", path, err)
	}
	if isBinary(data) {
		return nil, fmt.Errorf("ctxread: %s 是二进制文件（%d 字节），不返回内容；请用专用工具处理", path, len(data))
	}
	return data, nil
}

// isBinary 用前 512 字节有没有 NUL 字节判断是不是二进制。
func isBinary(data []byte) bool {
	n := len(data)
	if n > 512 {
		n = 512
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

// ChooseMode 按文件类型和大小选分档（抄 lean-ctx 的 chooseReadMode，v2 修过）。
// 先判存在与目录，再按大小分档；文档/数据文件超过 DocMapMinBytes 也走 ModeMap。
func ChooseMode(path string) (Mode, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("ctxread: 文件不存在或不可读 %s: %w", path, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("ctxread: %s 是目录，不是文件", path)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("ctxread: %s 不是普通文件（设备文件或管道），拒绝读取", path)
	}
	size := st.Size()
	ext := strings.ToLower(filepath.Ext(path))
	if fullReadExtensions[ext] {
		if size > DocMapMinBytes {
			return ModeMap, nil
		}
		return ModeFull, nil
	}
	if !codeExtensions[ext] {
		if size > NonCodeMapMinBytes {
			return ModeMap, nil
		}
		return ModeFull, nil
	}
	if size >= CodeSignaturesMinBytes {
		return ModeSignatures, nil
	}
	if size >= CodeFullReadMaxBytes {
		return ModeMap, nil
	}
	return ModeFull, nil
}

// Read 读一个文件。mode 为 ModeAuto 时自动分档；其它值强制用指定方式。
// 所有出口统一过 DefaultMaxBytes 硬上限；返回的文本末尾带压缩脚注。
func Read(path string, mode Mode) (Result, error) {
	if mode == ModeAuto {
		m, err := ChooseMode(path)
		if err != nil {
			return Result{}, err
		}
		mode = m
	}
	data, err := readFileSafe(path)
	if err != nil {
		return Result{}, err
	}
	origTokens := estimateTokens(string(data))
	lines := countLines(data)

	var text string
	truncated := false
	switch mode {
	case ModeFull:
		text, truncated = truncateText(string(data), DefaultMaxLines)
	case ModeFullCompact:
		text, truncated = truncateText(compactText(string(data)), DefaultMaxLines)
	case ModeMap:
		text = buildMap(path, data)
	case ModeSignatures:
		text = buildSignatures(path, data)
	case ModeRaw:
		text, truncated = truncateText(string(data), DefaultMaxLines)
	default:
		return Result{}, fmt.Errorf("ctxread: 未知的读方式 %q", mode)
	}

	// 统一出口：所有方式的结果都过一次硬字节上限（v2 堵漏洞 1）。
	if capped, cut := capBytes(text, DefaultMaxBytes); cut {
		text = capped + "\n\n[截断：输出超过 24KB 上限，已按字节截断]"
		truncated = true
	}

	outTokens := estimateTokens(text)
	text += footer(origTokens, outTokens)
	return Result{Mode: mode, Text: text, Lines: lines, Truncated: truncated, OrigTokens: origTokens, OutTokens: outTokens}, nil
}

// ReadLines 只读指定行号窗口（1 起）。这是看全文的唯一口子，一次最多 DefaultMaxLines 行、24KB。
// 模型在 map/signatures 里看到想看的行号，就用这个取那几行。
func ReadLines(path string, start, end int) (Result, error) {
	data, err := readFileSafe(path)
	if err != nil {
		return Result{}, err
	}
	origTokens := estimateTokens(string(data))
	total := countLines(data)
	if total == 0 {
		return Result{Mode: ModeFull, Text: "(空文件)", Lines: 0, Truncated: false, OrigTokens: 0, OutTokens: 0}, nil
	}
	if start < 1 {
		start = 1
	}
	if end <= 0 || end > total {
		end = total
	}
	if end-start+1 > DefaultMaxLines {
		end = start + DefaultMaxLines - 1
	}
	if start > end {
		return Result{}, fmt.Errorf("ctxread: 行号窗口无效 %d-%d（文件共 %d 行）", start, end, total)
	}
	all := strings.Split(string(data), "\n")
	sel := strings.Join(all[start-1:end], "\n")
	truncated := end < total
	// 统一出口：行号窗口的结果也过硬字节上限（v2 堵漏洞 1）。
	if capped, cut := capBytes(sel, DefaultMaxBytes); cut {
		sel = capped + "\n\n[截断：输出超过 24KB 上限，已按字节截断]"
		truncated = true
	}
	outTokens := estimateTokens(sel)
	text := sel + footer(origTokens, outTokens)
	return Result{Mode: ModeFull, Text: text, Lines: total, Truncated: truncated, OrigTokens: origTokens, OutTokens: outTokens}, nil
}

// countLines 数文件总行数（空文件返回 0）。
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	return bytes.Count(data, []byte("\n")) + 1
}

// truncateText 只按行数截断（v3 修：字节截断统一由 Read 出口的 capBytes 干一次，不重复截，
// 之前 truncateText 里先截字节拼提示、出口 capBytes 再切一次，会把提示语切碎）。
func truncateText(s string, maxLines int) (string, bool) {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s, false
	}
	out := strings.Join(lines[:maxLines], "\n")
	out += "\n\n[截断：按行超过上限，只显示部分内容]"
	return out, true
}

// compactText 压掉多余空行和行尾空格（full-compact 用）。
// v2：改用 strings.Split，不用 bufio.Scanner，不丢超长单行。
func compactText(s string) string {
	var b strings.Builder
	empty := 0
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimRight(raw, " \t")
		if strings.TrimSpace(line) == "" {
			empty++
			if empty > 1 {
				continue
			}
		} else {
			empty = 0
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// capLine 单行长度上限：超 MaxLineBytes 的行切掉尾部并标注。
func capLine(s string) string {
	if len(s) <= MaxLineBytes {
		return s
	}
	cut, _ := capBytes(s, MaxLineBytes)
	return cut + "…[行太长已截断]"
}

// buildMap 按文件类型给结构图。
func buildMap(path string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case ext == ".go":
		return goOutline(path, data)
	case ext == ".json":
		return jsonOutline(data)
	case ext == ".md":
		return mdOutline(data)
	case ext == ".yaml" || ext == ".yml":
		return yamlOutline(data)
	case codeExtensions[ext]:
		return genericOutline(ext, data)
	default:
		return headOutline(data)
	}
}

// buildSignatures 按文件类型给签名。
func buildSignatures(path string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case ext == ".go":
		return goSignatures(path, data)
	case codeExtensions[ext]:
		return genericSignatures(ext, data)
	default:
		// 非代码文件没有签名的概念，退回结构图。
		return buildMap(path, data)
	}
}

// ---- Go 文件：用标准库 go/parser 精确抽取（Go 写的最大好处，不用正则猜）----

// goOutline 给 Go 文件的结构图：包名、引入、每个顶层声明一行（行号 + 种类 + 名字）。
func goOutline(path string, data []byte) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		return headOutline(data)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n", f.Name.Name)
	if len(f.Imports) > 0 {
		b.WriteString("imports:\n")
		for _, im := range f.Imports {
			fmt.Fprintf(&b, "  %s\n", strings.Trim(im.Path.Value, `"`))
		}
	}
	b.WriteString("outline:\n")
	for _, decl := range f.Decls {
		pos := fset.Position(decl.Pos())
		switch d := decl.(type) {
		case *ast.FuncDecl:
			recv := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv = "(" + exprString(fset, d.Recv.List[0].Type) + ") "
			}
			fmt.Fprintf(&b, "  %d: func %s%s\n", pos.Line, recv, d.Name.Name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					fmt.Fprintf(&b, "  %d: type %s\n", fset.Position(s.Pos()).Line, s.Name.Name)
				case *ast.ValueSpec:
					for _, name := range s.Names {
						fmt.Fprintf(&b, "  %d: %s %s\n", fset.Position(s.Pos()).Line, d.Tok.String(), name.Name)
					}
				}
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// goSignatures 给 Go 文件的签名：函数完整签名（带起止行号）、类型定义（含结构体字段与泛型参数），不要函数体。
func goSignatures(path string, data []byte) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		return headOutline(data)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", f.Name.Name)
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			start := fset.Position(d.Pos()).Line
			end := fset.Position(d.End()).Line
			var sb strings.Builder
			printer.Fprint(&sb, fset, d.Type)
			sig := strings.TrimSpace(sb.String())
			recv := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv = "func (" + exprString(fset, d.Recv.List[0].Type) + ") "
			} else {
				recv = "func "
			}
			fmt.Fprintf(&b, "// L%d-L%d\n%s%s%s\n\n", start, end, recv, d.Name.Name, strings.TrimPrefix(sig, "func"))
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					// v2：直接打整个 TypeSpec，保住泛型参数 [T any] 和类型别名 =。
					var sb strings.Builder
					printer.Fprint(&sb, fset, s)
					start := fset.Position(s.Pos()).Line
					end := fset.Position(s.End()).Line
					fmt.Fprintf(&b, "// L%d-L%d\ntype %s\n\n", start, end, strings.TrimSpace(sb.String()))
				case *ast.ValueSpec:
					// v2：导出（首字母大写）的顶层 const/var 只打一行名字 + 行号。
					for _, name := range s.Names {
						if name.IsExported() {
							fmt.Fprintf(&b, "// L%d\n%s %s\n\n", fset.Position(s.Pos()).Line, d.Tok.String(), name.Name)
						}
					}
				}
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// exprString 把一个 AST 表达式打回源码文本。
func exprString(fset *token.FileSet, e ast.Expr) string {
	var sb strings.Builder
	printer.Fprint(&sb, fset, e)
	return strings.TrimSpace(sb.String())
}

// ---- 其它语言：按后缀分流正则（v2：不再所有语言跑同一套，避免误伤）----

// outlineRules 按后缀给结构图用的正则。
func outlineRules(ext string) []outlineRule {
	switch ext {
	case ".py":
		return []outlineRule{{"def", rePyDef}}
	case ".js", ".jsx", ".ts", ".tsx":
		return []outlineRule{
			{"function", reJSFunc},
			{"class", reJSClass},
			{"const", reJSConst},
			{"method", reJSMethod},
		}
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".java", ".cs":
		return []outlineRule{{"func", reCFunc}}
	case ".sh", ".bash", ".zsh":
		return []outlineRule{{"func", reShFunc}}
	case ".sql":
		return []outlineRule{{"object", reSQLObj}}
	default:
		return []outlineRule{
			{"def", rePyDef},
			{"function", reJSFunc},
			{"class", reJSClass},
		}
	}
}

// matchName 对一行跑一条规则，返回（种类，名字，是否匹配）。
// 名字是关键字（if/for/return 等）时不算匹配；Python 的种类按实际关键字取。
func matchName(rule outlineRule, line string) (string, string, bool) {
	m := rule.re.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	name := lastNonEmpty(m[1:])
	if name == "" {
		return "", "", false
	}
	if cKeywords[strings.ToLower(name)] {
		return "", "", false
	}
	kind := rule.kind
	if rule.re == rePyDef {
		// m[1] 是关键字（async def / def / class）。
		if strings.Contains(m[1], "def") {
			kind = "def"
		} else {
			kind = "class"
		}
	}
	return kind, name, true
}

// genericOutline 给非 Go 代码文件的结构图：每行一个顶层定义（行号 + 名字）。
// v2：按后缀分流正则；Python 允许前导缩进（保留层级）。
func genericOutline(ext string, data []byte) string {
	lines := strings.Split(string(data), "\n")
	rules := outlineRules(ext)
	var b strings.Builder
	b.WriteString("outline:\n")
	found := 0
	for i, line := range lines {
		for _, rule := range rules {
			kind, name, ok := matchName(rule, line)
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "  %d: %s%s %s\n", i+1, indentPrefix(line), kind, name)
			found++
			break
		}
	}
	if found == 0 {
		return headOutline(data)
	}
	return strings.TrimRight(b.String(), "\n")
}

// genericSignatures 给非 Go 代码文件的签名：有签名的那几行原文，带行号（v3 修：之前没打行号，
// 模型看 96KB 以上的 Python/JS 签名不知道在第几行，没法接 ReadLines）。每行过单行长度上限。
func genericSignatures(ext string, data []byte) string {
	lines := strings.Split(string(data), "\n")
	rules := outlineRules(ext)
	var out []string
	for i, line := range lines {
		for _, rule := range rules {
			_, _, ok := matchName(rule, line)
			if !ok {
				continue
			}
			out = append(out, fmt.Sprintf("%d: %s", i+1, capLine(strings.TrimRight(line, " \t"))))
			break
		}
	}
	if len(out) == 0 {
		return headOutline(data)
	}
	return strings.Join(out, "\n")
}

// lastNonEmpty 返回匹配组里最后一个非空串（不同正则的捕获组位置不一样）。
func lastNonEmpty(groups []string) string {
	for i := len(groups) - 1; i >= 0; i-- {
		if groups[i] != "" {
			return groups[i]
		}
	}
	return ""
}

// indentPrefix 返回一行的前导空白（制表符换成两格），用来在结构图里显示层级。
func indentPrefix(line string) string {
	var b strings.Builder
	for _, r := range line {
		if r == ' ' {
			b.WriteByte(' ')
		} else if r == '\t' {
			b.WriteString("  ")
		} else {
			break
		}
	}
	return b.String()
}

// ---- 数据和文档文件 ----

// jsonOutline 给 JSON 文件的结构图：顶层键 + 值类型。
func jsonOutline(data []byte) string {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return headOutline(data)
	}
	var b strings.Builder
	b.WriteString("outline:\n")
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s: %s\n", k, jsonTypeName(t[k]))
		}
	case []interface{}:
		fmt.Fprintf(&b, "  [array] len=%d, item: %s\n", len(t), jsonTypeName(firstNonNil(t)))
	default:
		fmt.Fprintf(&b, "  %s\n", jsonTypeName(v))
	}
	return strings.TrimRight(b.String(), "\n")
}

func jsonTypeName(v interface{}) string {
	switch t := v.(type) {
	case map[string]interface{}:
		return fmt.Sprintf("object(%d keys)", len(t))
	case []interface{}:
		return fmt.Sprintf("array(%d)", len(t))
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return "value"
	}
}

func firstNonNil(arr []interface{}) interface{} {
	for _, v := range arr {
		if v != nil {
			return v
		}
	}
	return nil
}

// yamlOutline 给 YAML 文件的结构图：顶层键（正则已提到包级预编译）。
func yamlOutline(data []byte) string {
	var b strings.Builder
	b.WriteString("outline:\n")
	found := 0
	for i, line := range strings.Split(string(data), "\n") {
		if m := reYAMLKey.FindStringSubmatch(line); m != nil {
			fmt.Fprintf(&b, "  %d: %s\n", i+1, m[1])
			found++
		}
	}
	if found == 0 {
		return headOutline(data)
	}
	return strings.TrimRight(b.String(), "\n")
}

// mdOutline 给 Markdown 文件的结构图：标题 + 行号（正则已提到包级预编译）。
func mdOutline(data []byte) string {
	var b strings.Builder
	b.WriteString("outline:\n")
	found := 0
	for i, line := range strings.Split(string(data), "\n") {
		if m := reMDHead.FindStringSubmatch(line); m != nil {
			fmt.Fprintf(&b, "  %d: %s %s\n", i+1, m[1], strings.TrimSpace(m[2]))
			found++
		}
	}
	if found == 0 {
		return headOutline(data)
	}
	return strings.TrimRight(b.String(), "\n")
}

// headOutline 兜底：给前 40 行 + 总行数。v2：每行过单行长度上限，防 5MB 单行文件。
func headOutline(data []byte) string {
	lines := strings.Split(string(data), "\n")
	n := len(lines)
	if n > 40 {
		lines = lines[:40]
	}
	for i, l := range lines {
		lines[i] = capLine(l)
	}
	out := strings.Join(lines, "\n")
	out += fmt.Sprintf("\n\n[共 %d 行，只显示前 40 行，用行号窗口取其余部分]", n)
	return out
}
