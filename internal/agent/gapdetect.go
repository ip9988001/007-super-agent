package agent

import (
	"fmt"
	"strings"
	"time"

	"007-super-agent/internal/forge"
)

// 缺口检测（第 23 步规格 §2）：主循环里的三条规则，只写缺口报告，
// 绝不打断当前任务。Forge 为 nil 时检测关闭（测试/旧调用方）。
//
// 规则 1：连续 3 次展开（expand）仍找不到合用工具 → no_tool；
// 规则 2：凑合完成：≥3 个工具绕弯完成、其中 ≥2 个是勉强匹配 → workaround；
// 规则 3：同一工具 7 天内 ≥3 次因“不支持”失败 → tool_unsupported。

// detectExpandGap 是规则 1：连续展开计数。reported 是本任务内
// 是否已报过（每任务只报一次）。
func (a *Agent) detectExpandGap(taskID, taskText string, history []stepRecord, reported *bool) {
	if a.Forge == nil || reported == nil || *reported {
		return
	}
	// 末尾连续成功的展开轮数。
	n := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].ToolID != "expand" || history[i].Status != "success" {
			break
		}
		n++
	}
	if n < 3 {
		return
	}
	// 证据：展开过的大类/子类。
	var expanded []string
	for _, s := range history {
		if s.ToolID == "expand" && s.Status == "success" {
			expanded = append(expanded, strings.Join(s.Inputs, "/"))
		}
	}
	id, isNew, err := a.Forge.WriteGap(forge.Gap{
		TaskID: taskID, TaskText: taskText, Trigger: "no_tool",
		Evidence: map[string]any{
			"consecutive_expands": n,
			"expanded":            strings.Join(expanded, ","),
		},
		CreatedTS: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	if isNew {
		_ = a.Forge.AppendEvent(forge.Event{
			Event: forge.EvGapReported, GapID: id,
			Input:  taskText,
			Output: fmt.Sprintf("规则 1：连续展开 %d 次仍无合用工具（%s）", n, strings.Join(expanded, ",")),
			Result: "ok",
		})
	}
	*reported = true
}

// detectWorkaroundGap 是规则 2：凑合完成。保守判定（宁可漏报）：
// 本任务用了 ≥3 个不同工具，且其中 ≥2 个在本任务里至少失败过一次
// （执行失败/无产出/参数失败——模型在勉强拼凑）。
func (a *Agent) detectWorkaroundGap(taskID, taskText string, history []stepRecord) {
	if a.Forge == nil {
		return
	}
	used := map[string]bool{}
	poor := map[string]bool{}
	for _, s := range history {
		if s.ToolID == "expand" {
			continue
		}
		used[s.ToolID] = true
		if s.Status != "success" {
			poor[s.ToolID] = true
		}
	}
	if len(used) < 3 || len(poor) < 2 {
		return
	}
	var usedList, poorList []string
	for id := range used {
		usedList = append(usedList, id)
	}
	for id := range poor {
		poorList = append(poorList, id)
	}
	id, isNew, err := a.Forge.WriteGap(forge.Gap{
		TaskID: taskID, TaskText: taskText, Trigger: "workaround",
		Evidence: map[string]any{
			"tools_used":   strings.Join(usedList, ","),
			"poor_matches": strings.Join(poorList, ","),
		},
		CreatedTS: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	if isNew {
		_ = a.Forge.AppendEvent(forge.Event{
			Event: forge.EvGapReported, GapID: id,
			Input: taskText,
			Output: fmt.Sprintf("规则 2：凑合完成，用了 %d 个工具、其中 %d 个勉强匹配（%s）",
				len(used), len(poor), strings.Join(poorList, ",")),
			Result: "ok",
		})
	}
}

// detectUnsupportedGap 是规则 3：同一工具 7 天内 ≥3 次因“不支持”
// 失败（执行账本 err_head 含 unsupported / not supported / 不支持）。
// 缺口文本用工具签名（同一工具 7 天内只报一次，由 WriteGap 去重兜底）。
func (a *Agent) detectUnsupportedGap(taskID string, history []stepRecord) {
	if a.Forge == nil || a.Led == nil {
		return
	}
	failed := map[string]bool{}
	for _, s := range history {
		if s.ToolID == "expand" {
			continue
		}
		if s.Status != "success" {
			failed[s.ToolID] = true
		}
	}
	for toolID := range failed {
		n, err := a.Led.CountUnsupportedFailures(toolID, 7)
		if err != nil || n < 3 {
			continue
		}
		taskText := fmt.Sprintf("工具 %s 在 7 天内 ≥3 次因不支持而失败", toolID)
		id, isNew, err := a.Forge.WriteGap(forge.Gap{
			TaskID: taskID, TaskText: taskText, Trigger: "tool_unsupported",
			Evidence: map[string]any{
				"tool_id":                 toolID,
				"unsupported_failures_7d": n,
			},
			CreatedTS: time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			continue
		}
		if isNew {
			_ = a.Forge.AppendEvent(forge.Event{
				Event: forge.EvGapReported, GapID: id,
				Input:  taskText,
				Output: fmt.Sprintf("规则 3：工具 %s 7 天内 %d 次因不支持而失败", toolID, n),
				Result: "ok",
			})
		}
	}
}

// detectGapsAtEnd 是任务结束时的检测：规则 2 只在任务完成时跑，
// 规则 3 无论完成与否都跑。
func (a *Agent) detectGapsAtEnd(taskID, taskText string, history []stepRecord, completed bool) {
	if completed {
		a.detectWorkaroundGap(taskID, taskText, history)
	}
	a.detectUnsupportedGap(taskID, history)
}
