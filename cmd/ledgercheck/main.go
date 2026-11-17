package main
import (
	"fmt"
	"os"

	"007-super-agent/internal/ledger"
)

func chk(err error) { if err != nil { panic(err) } }

func main() {
	l, err := ledger.Open(os.Args[1])
	chk(err)
	defer ledger.Close(l)
	id, err := l.RecordExecution(ledger.ExecutionRecord{SessionID: "s1", TaskID: "t1", Command: "echo hi", ExitCode: 0, Status: "success"})
	chk(err)
	rows, err := l.ListExecutions("t1", 10)
	chk(err)
	fmt.Printf("wrote id=%d read back: %+v\n", id, rows[0])
}
