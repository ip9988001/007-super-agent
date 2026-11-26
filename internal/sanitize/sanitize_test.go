package sanitize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件全部用合成假密钥做测试，绝不用真实密钥值。

func TestSanitizeKeyValue(t *testing.T) {
	in := "配置：api_key = FAKEKEY1234567890abcdef，password: FakePass123456"
	out := Sanitize(in)
	if strings.Contains(out, "FAKEKEY") || strings.Contains(out, "FakePass") {
		t.Fatalf("键值未脱掉：%q", out)
	}
	if !strings.Contains(out, "api_key = "+Redacted) || !strings.Contains(out, "password: "+Redacted) {
		t.Fatalf("键名应保留、值应遮掉：%q", out)
	}
}

func TestSanitizePrefixedTokens(t *testing.T) {
	// 合成假值：前缀格式真、内容全假。
	in := "key 是 sk-FAKEKEY1234567890abcdef，gh 是 ghp_FAKEFAKEFAKEFAKE1234567890，aws 是 AKIAFAKEFAKEFAKEFAKE"
	out := Sanitize(in)
	for _, fake := range []string{"sk-FAKE", "ghp_FAKE", "AKIAFAKE"} {
		if strings.Contains(out, fake) {
			t.Fatalf("前缀 token 未脱掉：%q", out)
		}
	}
	if strings.Count(out, Redacted) != 3 {
		t.Fatalf("应恰好遮 3 处，得 %q", out)
	}
}

func TestSanitizeHighEntropy(t *testing.T) {
	// 合成高熵长串（随机感字符拼成，非真实凭证）。
	in := "token 是 Xy9QwErTyUiOpAsDfGhJkLzXcVbNm1234567890"
	out := Sanitize(in)
	if strings.Contains(out, "Xy9QwErTyUiOpAsDfGhJkLzXcVbNm") {
		t.Fatalf("高熵长串未脱掉：%q", out)
	}
}

func TestSanitizeLongHex(t *testing.T) {
	// 合成 40 位 hex 假串（Cloudflare Global API Key 同类形态）。
	in := "key 是 abcdef0123456789abcdef0123456789abcdef01"
	out := Sanitize(in)
	if strings.Contains(out, "abcdef0123456789") {
		t.Fatalf("长 hex 串未脱掉：%q", out)
	}
}

func TestSanitizeBearerAndPrivateKeyAndURL(t *testing.T) {
	in := "Authorization: Bearer FAKEBEARER1234567890abcdef\n" +
		"-----BEGIN FAKE PRIVATE KEY-----\nZmFrZWZha2VmYWtlZmFrZWZha2U=\n-----END FAKE PRIVATE KEY-----\n" +
		"地址 https://fakeuser:FakePass123456@example.com/x"
	out := Sanitize(in)
	for _, fake := range []string{"FAKEBEARER", "ZmFrZWZha2VmYWtl", "fakeuser:FakePass"} {
		if strings.Contains(out, fake) {
			t.Fatalf("未脱掉：%q", out)
		}
	}
	if !strings.Contains(out, "Bearer "+Redacted) || !strings.Contains(out, "https://"+Redacted+"@example.com/x") {
		t.Fatalf("前缀应保留：%q", out)
	}
}

func TestSanitizeLeavesNormalText(t *testing.T) {
	in := "今天把 Cloudflare 注册流程走通了，邮箱验证花了三分钟，端口 6262 正常监听，记得下次先备份再改配置。"
	if got := Sanitize(in); got != in {
		t.Fatalf("正常文本不应被改：%q", got)
	}
}

func TestSanitizeIdempotent(t *testing.T) {
	in := "api_key=FAKEKEY1234567890abcdef 高熵 Xy9QwErTyUiOpAsDfGhJkLzXcVbNm1234567890"
	once := Sanitize(in)
	if twice := Sanitize(once); twice != once {
		t.Fatalf("脱敏须幂等：%q vs %q", once, twice)
	}
}

func TestContainsSecret(t *testing.T) {
	if !ContainsSecret("token=FAKEVALUE1234567890abcdef") {
		t.Fatal("应检出凭证串")
	}
	if ContainsSecret("今天天气不错，适合出门散步。") {
		t.Fatal("正常文本不应检出")
	}
}

func TestSanitizeForSummaryReady(t *testing.T) {
	// 节点 2（简介入库前）调用点已就绪：同一规则，待第二阶段接入。
	if got := SanitizeForSummary("api_key=FAKEKEY1234567890abcdef"); !strings.Contains(got, Redacted) {
		t.Fatalf("简介脱敏入口未生效：%q", got)
	}
}

func TestScanDirAndRedactFile(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "a.md")
	cleanFile := filepath.Join(dir, "sub", "b.md")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// 合成假密钥写入临时文件（测试专用，用完即删）。
	if err := os.WriteFile(secretFile, []byte("# 假记录\napi_key=FAKEKEY1234567890abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cleanFile, []byte("# 干净记录\n今天完成了备份。\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FilesScanned != 2 || rep.FilesWithSecrets != 1 || rep.TotalHits < 1 {
		t.Fatalf("巡检计数不对：%+v", rep)
	}
	changed, err := RedactFile(secretFile)
	if err != nil || !changed {
		t.Fatalf("RedactFile 应改动含凭证文件：changed=%v err=%v", changed, err)
	}
	after, _ := os.ReadFile(secretFile)
	if ContainsSecret(string(after)) {
		t.Fatal("重写后仍有残留")
	}
	changed, err = RedactFile(cleanFile)
	if err != nil || changed {
		t.Fatalf("干净文件不应被改动：changed=%v err=%v", changed, err)
	}
}
