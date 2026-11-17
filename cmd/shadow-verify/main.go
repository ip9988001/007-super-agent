// Command shadow-verify 回放 10 条样本信：影子模式下分诊与“会跑什么”，不真执行。
package main
import("fmt";"log";"os";"strings";"007-super-agent/internal/ledger";"007-super-agent/internal/registry";"007-super-agent/internal/triage")
func stub(p string)(string,int64,int64,error){if i:=strings.Index(p,"候选工具");i>=0{s:=p[i:];s=s[strings.Index(s,"：")+3:];s=s[:strings.Index(s,"\n")];return `{"route":"TOOL","tool_id":"`+strings.Split(s,", ")[0]+`","reason":"候选集首选"}`,10,10,nil};u:=p[strings.LastIndex(p,"用户原话：")+len("用户原话："):];if strings.Contains(u,"天气")||strings.Contains(u,"你是谁"){return `{"route":"DIRECT","tool_id":"","reason":"常识问答"}`,10,10,nil};if strings.Contains(u,"定时任务")||strings.Contains(u,"备份"){return `{"route":"META","tool_id":"","reason":"改自身配置"}`,10,10,nil};if strings.Contains(u,"playbook"){return `{"route":"TOOL","tool_id":"cap-memory-playbooks","reason":""}`,10,10,nil};return `{"route":"TOOL","tool_id":"cap-does-not-exist","reason":""}`,10,10,nil}
func main(){
rg,err:=registry.Load(os.Getenv("DATA_DIR"));if err!=nil{log.Fatal(err)}
dir,_:=os.MkdirTemp("","shverify");l,err:=ledger.Open(dir+"/ledger.db");if err!=nil{log.Fatal(err)};defer ledger.Close(l)
for i,t:=range []string{"/audit 审一下这段代码","/rea 帮我复盘一下上周","今天天气怎么样","你好，你是谁","把你自己的定时任务改成早上 8 点","把备份改成每天凌晨 3 点跑","/seo 优化一下这篇文章","/graph 画一张架构图","查一下那个下架的 playbook 工具","用白皮书工具帮我生成一份文档"}{
d,err:=triage.Triage(rg,l,triage.Input{Text:t,TaskID:fmt.Sprintf("sh-%02d",i+1),SessionID:"shadow-verify"},stub);if err!=nil{fmt.Printf("%s => ERROR: %v\n",t,err);continue}
if d.Route!=triage.RouteTool{fmt.Printf("%s => route=%s tier=%d\n",t,d.Route,d.Tier);continue}
e,l3,err:=triage.ResolveTool(rg,d.ToolID);if err!=nil{fmt.Printf("%s => ERROR: %v\n",t,err);continue}
_,err=l.RecordExecution(ledger.ExecutionRecord{TaskID:fmt.Sprintf("sh-%02d",i+1),SessionID:"shadow-verify",ToolID:d.ToolID,Command:"shadow: "+e.EntryPoint,Status:"shadow",AdvisorVerdict:"NOTE",AdvisorNote:"影子回放：只记不跑"});if err!=nil{log.Fatal(err)}
fmt.Printf("%s => route=%s tool=%s shadow: would run %s (L3 %d chars)\n",t,d.Route,d.ToolID,e.EntryPoint,len([]rune(l3)))}
}
