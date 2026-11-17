// Command registry-verify 验证 Step 4：加载总账、取 L1/L2/L3、确认 available:false 不可调用。
// 用法：DATA_DIR=/root/super-agent-007/data ./registry-verify
package main

import (
	"007-super-agent/internal/registry"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	chk := func(e error) {
		if e != nil {
			log.Fatal(e)
		}
	}
	r, e := registry.Load(os.Getenv("DATA_DIR"))
	chk(e)
	l1, e := r.L1()
	chk(e)
	l2, e := r.L2("fetch")
	chk(e)
	l3, e := r.L3("cap-fetch-curl-jq")
	chk(e)
	fmt.Printf("entries=%d L1=%d L2=%d L3=%d\n", r.Count(), strings.Count(l1, "\n"), strings.Count(l2, "\n"), len([]rune(l3)))
	fmt.Println("callable cap-memory-playbooks:", r.Callable("cap-memory-playbooks"))
}
