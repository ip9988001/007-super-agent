package main
import (
	"fmt"
	"log"
	"os"
	"strings"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/triage"
)

// stubModelCall 是 Tier-1 的确定性桩：生产上换成真实模型调用。
func stubModelCall(p string) (string, int64, int64, error) {
	if strings.Contains(p, "天气") {
		return `{"route":"DIRECT","tool_id":"","reason":"常识问答"}`, 120, 20, nil
	}
	return `{"route":"META","tool_id":"","reason":"改自身定时任务"}`, 120, 20, nil
}
func main() {
	rg, err := registry.Load(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	for _, text := range []string{"/audit 审一下这段代码", "帮我查一下今天天气怎么样", "把你自己的定时任务改成早上 8 点"} {
		d, err := triage.Triage(rg, nil, triage.Input{Text: text}, stubModelCall)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s => route=%s tool_id=%s tier=%d\n", text, d.Route, d.ToolID, d.Tier)
	}
}
