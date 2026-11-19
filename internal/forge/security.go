package forge

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// 三阶轻量静态检查（规格 §2A.5）：规则扫描安装文件与入口。
// 必查：install.sh、setup.py / pyproject.toml、package.json 的
// postinstall 钩子及主入口。规则目标：curl|bash 下载执行、可疑外连、
// 读取 ~/.ssh / ~/.aws、全量环境变量上传。
// 命中返回 findings（每条“文件:模式”）；无命中返回空。命中不等于
// 直接淘汰——记入红线 R2，由锻造侧结合监工判定；可疑安装脚本的
// 候选在检索层已由 SecurityFlag 标出，这里是 staging 的复核。

// scanTargets 是必查的文件名（大小写不敏感匹配）。
var scanTargets = []string{
	"install.sh", "setup.py", "pyproject.toml", "package.json",
	"postinstall.js", "main.py", "index.js", "cli.py", "tool.py",
	"main.go", "index.ts", "cli.js",
}

// scanPatterns 是扫描模式（小写匹配）：模式 → 说明。
// scanRegexps 是正则模式（管道下载执行：curl/wget ... | bash/sh，
// 中间可隔 URL 与参数，字面匹配拦不住）。
var scanRegexps = []struct {
	re   *regexp.Regexp
	desc string
}{
	{regexp.MustCompile(`(?i)(curl|wget)[^|\n]*\|\s*(bash|sh)\b`), "curl/wget 管道 bash/sh 下载执行"},
}

// scanPatterns 是字面子串模式。
var scanPatterns = []struct {
	pattern string
	desc    string
}{
	{".ssh", "读取 ~/.ssh"},
	{".aws", "读取 ~/.aws"},
	{"os.environ", "读取全量环境变量（python）"},
	{"process.env", "读取全量环境变量（node）"},
	{"eval(", "eval 动态执行"},
}

// ScanStagingFiles 对 staging 目录里的必查文件做规则扫描。
// 返回 findings（“文件:说明”）；目录不存在返回错误。
func ScanStagingFiles(dir string) ([]string, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	var findings []string
	err := filepath.WalkDir(dir, func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		base := strings.ToLower(de.Name())
		target := false
		for _, t := range scanTargets {
			if base == t {
				target = true
				break
			}
		}
		if !target {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lower := strings.ToLower(string(b))
		rel, _ := filepath.Rel(dir, path)
		for _, p := range scanPatterns {
			if strings.Contains(lower, strings.ToLower(p.pattern)) {
				findings = append(findings, rel+": "+p.desc)
			}
		}
		for _, p := range scanRegexps {
			if p.re.MatchString(string(b)) {
				findings = append(findings, rel+": "+p.desc)
			}
		}
		return nil
	})
	return findings, err
}

// ScanText 对一段文本做同样的规则扫描（检索层深挖时用：对取回的
// README/依赖清单做轻量扫描，见 retrieval.scanSuspicious 的对应版）。
// 返回命中的说明（逗号分隔，无则空串）。
func ScanText(content string) string {
	lower := strings.ToLower(content)
	var hits []string
	for _, p := range scanPatterns {
		if strings.Contains(lower, strings.ToLower(p.pattern)) {
			hits = append(hits, p.desc)
		}
	}
	for _, p := range scanRegexps {
		if p.re.MatchString(content) {
			hits = append(hits, p.desc)
		}
	}
	return strings.Join(hits, ",")
}
