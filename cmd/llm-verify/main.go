// Command llm-verify 是第 10 步的验证程序（≤30 行）：桩服务器模拟一次模型分诊（应得 DIRECT）与一次参数化调用（只打印命令，不真跑）。
package main

import (
	"007-super-agent/internal/llm"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/triage"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":\"DIRECT\",\"tool_id\":\"\",\"reason\":\"常识直接答\"}"}}],"usage":{"prompt_tokens":100,"prompt_cache_hit_tokens":90,"prompt_cache_miss_tokens":10,"completion_tokens":20}}`))
	}))
	defer srv.Close()
	c := llm.New("deepseek-flash", "test-key")
	c.BaseURL = srv.URL
	reg, err := registry.Load("/root/super-agent-007/data")
	if err != nil {
		reg, err = registry.Load("step1")
	}
	if err != nil {
		fmt.Println("registry:", err)
		os.Exit(1)
	}
	reply, _, _, err := c.TriageCall("v1", triage.BuildTriageFrozen(reg, nil), triage.BuildTriageTail("帮我查一下今天天气怎么样"))
	if err != nil {
		fmt.Println("triage call:", err)
		os.Exit(1)
	}
	dec, err := triage.ParseTriageReply(reply, reg)
	fmt.Println("triage:", dec.Route, dec.Tier, err)
	argv, err := llm.BuildArgv(reg, "cap-code-semgrep", []string{"--limit", "5"})
	fmt.Println("argv:", argv, err)
}
