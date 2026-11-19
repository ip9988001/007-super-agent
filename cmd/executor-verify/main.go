package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
)

type stub struct{}

func (stub) CollectEvidenceBundle(t executor.Task, _ executor.Trigger) (executor.Evidence, error) {
	return executor.Evidence{TaskText: t.Text}, nil
}
func (stub) Check(e executor.Evidence, tr executor.Trigger) (executor.CheckResult, error) {
	return executor.CheckResult{Verdict: executor.VerdictNote, Note: "路径确认无误"}, nil
}

func main() {
	dir, _ := os.MkdirTemp("", "exec-verify")
	defer os.RemoveAll(dir)
	l, err := ledger.Open(dir + "/ledger.db")
	if err != nil {
		log.Fatal(err)
	}
	defer ledger.Close(l)
	ex := &executor.Executor{Ledger: l, Advisor: stub{}}
	r, _, err := ex.Run(executor.RunRequest{TaskID: "v1", SessionID: "s1", Argv: []string{"echo", "hi"}, Timeout: 10 * time.Second})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("echo exit=%d stdout=%q\n", r.ExitCode, r.StdoutTail)
	_, h, err := ex.Run(executor.RunRequest{TaskID: "v2", SessionID: "s1", Argv: []string{"sleep", "40"}, Timeout: 60 * time.Second})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("sleep40 handle in %s task=%s\n", time.Since(h.StartedAt), h.TaskID)
	h.Cancel()
	_, _ = h.Wait()
	tmp := dir + "/del.txt"
	_ = os.WriteFile(tmp, []byte("x"), 0600)
	r, _, err = ex.Run(executor.RunRequest{TaskID: "v3", SessionID: "s1", Argv: []string{"rm", tmp}, Timeout: 10 * time.Second, Irreversible: true, IrreversibleKind: executor.KindDeleteFile, Text: "删临时文件"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("rm verdict=%s exit=%d\n", r.AdvisorVerdict, r.ExitCode)
}
