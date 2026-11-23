// Command ctxread 是 007 执行端压缩读的命令行入口：
// 007 的工具注册表里 cap-ops-lean-ctx 的 entry_point 指向它，
// 执行会话的模型调它读文件；前置会话的 doRead 直接调 ctxread 包，
// 不走这个命令行。
//
// 用法：
//   ctxread <path>                     # 自动分档压缩读（默认）
//   ctxread lines <start> <end> <path> # 读指定行号窗口（end<=0 表示读到末尾）
//   ctxread mode <mode> <path>         # 强制分档：map / signatures / full / raw
//   ctxread status                     # 探针：打印版本与 OK
//
// 输出：读到的内容 + 脚注（Compressed X → Y tokens (-Z%)）。
// 失败：stderr 打印原因，退出码 1。
package main

import (
	"fmt"
	"os"
	"strconv"

	"007-super-agent/internal/ctxread"
)

const version = "ctxread v3 (2026-11-23)"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法：ctxread <path> | ctxread lines <start> <end> <path> | ctxread mode <mode> <path> | ctxread status")
		os.Exit(1)
	}
	if args[0] == "status" {
		fmt.Println(version, "OK")
		return
	}
	if args[0] == "lines" {
		if len(args) != 4 {
			fmt.Fprintln(os.Stderr, "用法：ctxread lines <start> <end> <path>")
			os.Exit(1)
		}
		start, err1 := strconv.Atoi(args[1])
		end, err2 := strconv.Atoi(args[2])
		if err1 != nil || err2 != nil {
			fmt.Fprintln(os.Stderr, "行号必须是数字")
			os.Exit(1)
		}
		res, err := ctxread.ReadLines(args[3], start, end)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(res.Text)
		return
	}
	if args[0] == "mode" {
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "用法：ctxread mode <mode> <path>（mode：map / signatures / full / raw）")
			os.Exit(1)
		}
		var m ctxread.Mode
		switch args[1] {
		case "map":
			m = ctxread.ModeMap
		case "signatures":
			m = ctxread.ModeSignatures
		case "full":
			m = ctxread.ModeFull
		case "raw":
			m = ctxread.ModeRaw
		default:
			fmt.Fprintln(os.Stderr, "mode 只能是 map / signatures / full / raw")
			os.Exit(1)
		}
		res, err := ctxread.Read(args[2], m)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(res.Text)
		return
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "用法：ctxread <path>")
		os.Exit(1)
	}
	res, err := ctxread.Read(args[0], ctxread.ModeAuto)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(res.Text)
}
