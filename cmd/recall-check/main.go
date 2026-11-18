package main

// 临时验证工具：对指定 data/memory/ 目录跑一次 Recall，打印前 3 条。
// 用法：recall-check <memoryDir> <query...>
import (
	"fmt"
	"os"
	"strings"

	"007-super-agent/internal/memory"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "用法：recall-check <memoryDir> <query>")
		os.Exit(1)
	}
	st, err := memory.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	q := strings.Join(os.Args[2:], " ")
	res, err := st.Recall(q)
	fmt.Printf("query=%q err=%v hits=%d\n", q, err, len(res))
	for _, r := range res {
		fmt.Printf("  [%s] score=%d text=%s\n", r.Source, r.Score, r.Text)
	}
}
