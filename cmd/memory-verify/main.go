package main

import (
	"fmt"
	"os"
	"strings"

	"007-super-agent/internal/memory"
)

func main() {
	dir, _ := os.MkdirTemp("", "mem-verify-*")
	defer os.RemoveAll(dir)
	st, err := memory.Open(dir)
	if err != nil {
		panic(err)
	}
	text := strings.Repeat("超级 Agent 记忆测试。", 600)
	err = st.WriteWiki(memory.WikiEntry{Kind: "concepts", Title: "测试词条", Summary: "记忆测试词条摘要", Body: text})
	fmt.Println("write wiki:", err)
	res, err := st.Recall("记忆测试")
	fmt.Println("recall:", len(res), "err:", err)
	for _, r := range res {
		fmt.Printf("  %s len=%d\n", r.Source, len([]rune(r.Text)))
	}
	sum, rolled, err := st.Rollover(210000, text)
	fmt.Println("rollover:", rolled, "summary runes:", len([]rune(sum)), "err:", err)
	chain, _ := st.ListCarryover()
	fmt.Println("carryover chain:", len(chain))
}
