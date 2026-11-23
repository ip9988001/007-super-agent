package front

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"007-super-agent/internal/dispatch"
)

// TestParseActions 单发与打包解析。
func TestParseActions(t *testing.T) {
	// 单发 reply。
	acts, err := parseActions(`{"action":"reply","text":"你好"}`)
	if err != nil {
		t.Fatalf("单发 reply: %v", err)
	}
	if len(acts) != 1 || acts[0].Action != "reply" || acts[0].Text != "你好" {
		t.Fatalf("单发 reply 解析不对：%+v", acts)
	}
	// 打包 recall + registry。
	acts, err = parseActions(`[{"action":"recall","query":"测试"},{"action":"registry","level":"L1"}]`)
	if err != nil {
		t.Fatalf("打包: %v", err)
	}
	if len(acts) != 2 || acts[0].Action != "recall" || acts[1].Action != "registry" {
		t.Fatalf("打包解析不对：%+v", acts)
	}
	// 打包里混 reply 必须报错。
	if _, err := parseActions(`[{"action":"recall","query":"x"},{"action":"reply","text":"y"}]`); err == nil {
		t.Fatalf("打包混 reply 应报错")
	}
	// 打包超 3 个必须报错。
	if _, err := parseActions(`[{"action":"recall","query":"a"},{"action":"recall","query":"b"},{"action":"recall","query":"c"},{"action":"recall","query":"d"}]`); err == nil {
		t.Fatalf("打包超 3 个应报错")
	}
	// 非法 JSON 必须报错。
	if _, err := parseActions(`不是 JSON`); err == nil {
		t.Fatalf("非法 JSON 应报错")
	}
	// 模型在 JSON 外包一层解释文字也能解析。
	acts, err = parseActions("好的，我先查一下：\n{\"action\":\"reply\",\"text\":\"你好\"}\n以上是我的决定。")
	if err != nil {
		t.Fatalf("包解释文字: %v", err)
	}
	if len(acts) != 1 || acts[0].Action != "reply" {
		t.Fatalf("包解释文字解析不对：%+v", acts)
	}
}

// TestValidateAction 各动作的必填字段。
func TestValidateAction(t *testing.T) {
	if err := validateAction(Action{Action: "reply", Text: "x"}); err != nil {
		t.Fatalf("reply 有效却报错: %v", err)
	}
	if err := validateAction(Action{Action: "reply"}); err == nil {
		t.Fatalf("reply 缺 text 应报错")
	}
	if err := validateAction(Action{Action: "dispatch", Route: "AGENT", Goal: "做事"}); err != nil {
		t.Fatalf("dispatch 有效却报错: %v", err)
	}
	if err := validateAction(Action{Action: "dispatch", Route: "WRONG", Goal: "做事"}); err == nil {
		t.Fatalf("dispatch route 非法应报错")
	}
	if err := validateAction(Action{Action: "registry", Level: "L2"}); err == nil {
		t.Fatalf("registry L2 缺 category 应报错")
	}
	if err := validateAction(Action{Action: "nonsense"}); err == nil {
		t.Fatalf("未知 action 应报错")
	}
}

// TestDoBashRejectsDangerous 只读 bash 必须拦住写操作与危险命令。
func TestDoBashRejectsDangerous(t *testing.T) {
	f := &Front{}
	ctx := context.Background()
	denied := []string{
		"rm -rf /tmp/x",             // 不在白名单
		"ls /tmp; rm -rf /tmp/x",    // 分号
		"ls /tmp | grep x",          // 管道
		"echo hi > /tmp/x",          // 重定向
		"cat /etc/passwd $(id)",     // 命令替换
		"sed -i s/a/b/ /tmp/x",      // sed 就地改
		"find /tmp -exec rm {} \\;", // find -exec
		"git push origin main",      // git 写操作
		"systemctl restart foo",     // systemctl 写操作
		"cat /tmp/x && echo hi",     // &&
		"ls `id`",                   // 反引号
	}
	for _, cmd := range denied {
		if _, err := f.doBash(ctx, cmd); err == nil {
			t.Errorf("只读 bash 应拒绝 %q，却放行了", cmd)
		}
	}
	// 白名单命令应放行（ls 当前目录一定成功）。
	if out, err := f.doBash(ctx, "ls"); err != nil {
		t.Fatalf("只读 bash 应放行 ls：%v", err)
	} else if strings.TrimSpace(out) == "" {
		t.Fatalf("ls 应有输出")
	}
	// git status 应放行（在仓库外会失败，但失败原因是退出码，不是拦截；
	// 这里只验证它不被“拒绝”——用 pwd 代替，pwd 一定成功）。
	if _, err := f.doBash(ctx, "pwd"); err != nil {
		t.Fatalf("只读 bash 应放行 pwd：%v", err)
	}
}

// TestDoReadCompressed 读文件走 ctxread：小文件全文，大文本文件给压缩读，
// 二进制与相对路径拒绝。
func TestDoReadCompressed(t *testing.T) {
	f := &Front{}
	dir := t.TempDir()
	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := f.doRead(small); err != nil || !strings.Contains(out, "hello") {
		t.Fatalf("小文件应读出 hello，得到 %q, %v", out, err)
	}
	// 大文本文件（约 13KB）不再拒绝，给压缩读（前 40 行 + 脚注）。
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("line of text content here\n", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := f.doRead(big)
	if err != nil {
		t.Fatalf("大文本文件应压缩读出，不应拒绝：%v", err)
	}
	if !strings.Contains(out, "line of text content here") {
		t.Fatalf("大文本文件压缩读应含正文，得到 %q", out)
	}
	// 二进制文件拒绝。
	bin := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(bin, make([]byte, 9*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.doRead(bin); err == nil {
		t.Fatalf("二进制文件应拒绝")
	}
	// 相对路径必须拒绝。
	if _, err := f.doRead("relative/path.txt"); err == nil {
		t.Fatalf("相对路径应拒绝")
	}
}

// TestDispatchTokens 派单 token 估算：短派单远小于 1000。
func TestDispatchTokens(t *testing.T) {
	d := &dispatch.Dispatch{
		UserText: "查一下今天的天气", Goal: "回答天气",
		Criteria: "给出温度", ToolIDs: []string{"cap-fetch-curl-jq"},
	}
	n := dispatchTokens(d)
	if n <= 0 || n > 100 {
		t.Fatalf("短派单 token 估算不合理：%d", n)
	}
}
