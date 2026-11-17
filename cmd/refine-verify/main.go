// Command refine-verify：写 3 行 L1、跑一次精炼（桩模型）、打印结果。
package main
import("context";"fmt";"log";"os";"strings";"time";"007-super-agent/internal/ledger";"007-super-agent/internal/memory";"007-super-agent/internal/watchdog")
func stub(_ context.Context, _ string, _ string)([]memory.WikiEntry,error){return []memory.WikiEntry{{Kind:memory.KindConcepts,Title:"测试词条",Summary:"桩模型提炼",Body:"正文"}},nil}
func main(){
dir,_:=os.MkdirTemp("","refverify");l,err:=ledger.Open(dir+"/ledger.db");if err!=nil{log.Fatal(err)};defer ledger.Close(l)
m,err:=memory.Open(dir+"/memory");if err!=nil{log.Fatal(err)}
for i:=0;i<3;i++{if err:=m.AppendL1(fmt.Sprintf("task t%d route=DIRECT status=ok duration_ms=%d",i+1,i+1));err!=nil{log.Fatal(err)}}
w:=watchdog.New(l,dir+"/ledger.db",nil);d:=time.Now().Format("2006-01-02")
r,err:=w.RefineDaily(context.Background(),m,d,stub);if err!=nil{log.Fatal(err)}
t,_:=m.ReadL1(d);fmt.Printf("refine: lines=%d written=%d failed=%d skipped=%v\nl1 lines in file: %d\n",r.Lines,r.Written,r.Failed,r.Skipped,strings.Count(t,"\n"))}
