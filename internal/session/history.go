package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
)

// RepairReport 是读回步历史时做的修复记录（规格 §4.3、§4.4）。
type RepairReport struct {
	TruncatedLines  int  // 截断的残行/坏行数（只截末尾残行 + 跳过中间坏行）
	ClosedToolCalls int  // 补闭合的 tool_calls 数
	DroppedMessages int  // 中途遇未闭合 assistant 而丢弃的消息数（防 400 的兜底）
	Repaired        bool // 是否做过任何修复
}

// AppendMessages 把消息追加进步历史文件（stepNo<=0 写 front.jsonl，
// 否则写 steps/step-<n>.jsonl）：一行一条 JSON，O_APPEND 只追加、
// 不改写历史行。单条消息超 MaxMessageBytes 时截断 content 并加标记。
// 写盘失败返回错误（调用方记日志，不杀任务）。
func (s *Store) AppendMessages(sessionID string, stepNo int, taskID string, msgs []llm.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	p, err := s.StepPath(sessionID, stepNo)
	if err != nil {
		return err
	}
	l := s.lockFor(sessionID)
	l.Lock()
	defer l.Unlock()
	// M-1：把本步完整输出的落盘路径（artifacts/<task>/step-<n>.txt）
	// 当提示传下去，瘦身时写进截断标记，续跑需要全文可按路径取。
	hint := ""
	if stepNo > 0 {
		hint = filepath.Join(s.dataDir, "artifacts", taskID, fmt.Sprintf("step-%d.txt", stepNo))
	}
	return s.appendLocked(p, msgs, hint)
}

// appendLocked 是 AppendMessages 的持锁版（Store 内部复用，调用方须已持锁）。
// fullOutputHint 是本步完整输出的落盘路径（M-1 瘦身标记用，可空）。
func (s *Store) appendLocked(p string, msgs []llm.Message, fullOutputHint string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("session: 建历史目录失败: %w", err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("session: 打开历史文件失败: %w", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, m := range msgs {
		mm := truncateMessage(m, fullOutputHint)
		b, err := json.Marshal(mm)
		if err != nil {
			return fmt.Errorf("session: 序列化历史消息失败: %w", err)
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return fmt.Errorf("session: 写历史失败: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("session: 刷历史失败: %w", err)
	}
	return nil
}

// truncateMessage 把单条消息的 content 截到 MaxMessageBytes 以内
// （按字节截，截断处加标记；tool_calls 的 arguments 很小，不截）。
// M-1：role:"tool" 的历史输出超 3KB 时再瘦一层——只留前 1500 字 +
// 末尾 500 字 + 截断标记（含原输出落盘路径提示，续跑需要全文可按
// 路径取）。当前轮执行内仍用完整输出，仅历史归档瘦身。
func truncateMessage(m llm.Message, fullOutputHint string) llm.Message {
	if m.Role == "tool" {
		if rs := []rune(m.Content); len(rs) > 3072 {
			marker := "\n……（历史归档截断：仅保留开头 1500 字与末尾 500 字，中间略去"
			if fullOutputHint != "" {
				marker += "；全文见 " + fullOutputHint
			}
			marker += "）\n"
			m.Content = string(rs[:1500]) + marker + string(rs[len(rs)-500:])
			return m
		}
	}
	if len(m.Content) > MaxMessageBytes {
		m.Content = m.Content[:MaxMessageBytes] + "\n……（本条消息存盘时截断）"
	}
	return m
}

// LoadMessages 读回一步的历史（断点续跑用）：
//   - 文件不存在 → (nil, nil, nil)。
//   - 逐行解析 JSON：末尾残行（kill -9 写半截）只截断残行，保留前面
//     全部完好行；中间坏行跳过并计数；修复后记 dispatch_event。
//   - 尾部闭合校验：最后一条带 tool_calls 的 assistant 消息若有未闭合
//     的调用，补一条 tool 消息（InterruptedToolNote）进返回结果，
//     并追加进文件（保持文件与内存一致），否则下一轮调 API 直接报 400。
//   - 中途（非尾部）出现未闭合的 assistant：把历史截到该条之前
//     （丢弃其后全部消息并重写文件），防 400 的兜底。
//
// 返回修复报告（无修复时为 nil）。全程持会话锁（读 + 修 + 写是原子的）。
func (s *Store) LoadMessages(sessionID string, stepNo int, taskID string) ([]llm.Message, *RepairReport, error) {
	p, err := s.StepPath(sessionID, stepNo)
	if err != nil {
		return nil, nil, err
	}
	l := s.lockFor(sessionID)
	l.Lock()
	defer l.Unlock()
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("session: 读历史文件失败: %w", err)
	}
	if len(raw) > MaxStepHistoryBytes {
		return nil, nil, fmt.Errorf("session: 历史文件过大（%d 字节），拒绝读入", len(raw))
	}
	rep := &RepairReport{}
	msgs, truncated, err := parseHistoryLines(raw, rep)
	if err != nil {
		return nil, nil, err
	}
	if truncated {
		// 只截末尾残行：把文件截到最后完好行之后。
		if terr := s.truncateHistoryFileLocked(p, raw); terr != nil {
			log.Printf("session %s 截断残行失败: %v", sessionID, terr)
		}
	}
	// 尾部闭合校验与中途截断。
	msgs, repaired := closeOpenToolCalls(msgs, rep)
	if repaired {
		// 补闭合的消息要追加进文件；中途截断要重写文件。
		if rep.DroppedMessages > 0 {
			if werr := s.rewriteHistoryFileLocked(p, msgs); werr != nil {
				log.Printf("session %s 重写历史文件失败: %v", sessionID, werr)
			}
		} else if rep.ClosedToolCalls > 0 {
			added := msgs[len(msgs)-rep.ClosedToolCalls:]
			if aerr := s.appendLocked(p, added, ""); aerr != nil {
				log.Printf("session %s 补写闭合消息失败: %v", sessionID, aerr)
			}
		}
	}
	if rep.Repaired {
		s.event(sessionID, taskID, ledger.DispatchEventSessionRepair,
			fmt.Sprintf("步 %d 历史修复：截断 %d 行，补闭合 %d 个调用，丢弃 %d 条", stepNo, rep.TruncatedLines, rep.ClosedToolCalls, rep.DroppedMessages))
	}
	return msgs, repOrNil(rep), nil
}

// repOrNil：无修复时返回 nil，调用方不用判空字段。
func repOrNil(rep *RepairReport) *RepairReport {
	if rep == nil || !rep.Repaired {
		return nil
	}
	return rep
}

// parseHistoryLines 逐行解析历史文件：空行跳过；坏行分两种——
// 在末尾（其后无完好行）算残行（返回 truncated=true，由调用方截文件）；
// 在中间算坏行（跳过并计数）。返回解析出的消息、是否含末尾残行、错误。
func parseHistoryLines(raw []byte, rep *RepairReport) ([]llm.Message, bool, error) {
	lines := bytes.Split(raw, []byte("\n"))
	var msgs []llm.Message
	truncated := false
	for i, ln := range lines {
		ln = bytes.TrimRight(ln, "\r")
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		var m llm.Message
		if err := json.Unmarshal(ln, &m); err != nil {
			// 其后是否还有完好行：有 → 中间坏行，跳过；无 → 末尾残行。
			restOK := false
			for _, later := range lines[i+1:] {
				if len(bytes.TrimSpace(later)) == 0 {
					continue
				}
				var probe llm.Message
				if json.Unmarshal(bytes.TrimRight(later, "\r"), &probe) == nil {
					restOK = true
					break
				}
			}
			if restOK {
				rep.TruncatedLines++
				rep.Repaired = true
				continue
			}
			rep.TruncatedLines += countNonEmptyLines(lines[i:])
			rep.Repaired = true
			truncated = true
			break
		}
		msgs = append(msgs, m)
	}
	return msgs, truncated, nil
}

// countNonEmptyLines 数一段里的非空行数。
func countNonEmptyLines(lines [][]byte) int {
	n := 0
	for _, ln := range lines {
		if len(bytes.TrimSpace(ln)) > 0 {
			n++
		}
	}
	return n
}

// truncateHistoryFileLocked 把历史文件截到最后完好行之后（持锁版）：
// 重新按行解析，只保留能解析的行（与 parseHistoryLines 同口径），原子改名写回。
func (s *Store) truncateHistoryFileLocked(p string, raw []byte) error {
	lines := bytes.Split(raw, []byte("\n"))
	var good [][]byte
	for _, ln := range lines {
		ln = bytes.TrimRight(ln, "\r")
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		var probe llm.Message
		if json.Unmarshal(ln, &probe) != nil {
			break
		}
		good = append(good, ln)
	}
	var buf bytes.Buffer
	for _, ln := range good {
		buf.Write(ln)
		buf.WriteByte('\n')
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// rewriteHistoryFileLocked 用给定的消息全量重写历史文件（持锁版，中途截断的兜底用）。
func (s *Store) rewriteHistoryFileLocked(p string, msgs []llm.Message) error {
	var buf bytes.Buffer
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// closeOpenToolCalls 检查历史里的 tool_calls 是否都闭合：
//   - 每条 assistant(tool_calls) 消息之后，必须有对应 tool_call_id 的
//     tool 消息，否则下一轮调 DeepSeek 直接报 400。
//   - 只修尾部：最后一条带 tool_calls 的 assistant 若有未闭合调用，
//     在末尾补 tool 消息（InterruptedToolNote）。
//   - 中途（非尾部）出现未闭合的 assistant：把历史截到该条之前
//     （丢弃其后全部消息），调用方据此重写文件。
//
// 返回修后消息与是否做过修复。
func closeOpenToolCalls(msgs []llm.Message, rep *RepairReport) ([]llm.Message, bool) {
	// 对每条 assistant(tool_calls)，检查其后（到下一条 assistant 之前）
	// 是否有全部对应的 tool 消息。
	for i, m := range msgs {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		// 下一条 assistant 的位置（或末尾）。
		next := len(msgs)
		for j := i + 1; j < len(msgs); j++ {
			if msgs[j].Role == "assistant" {
				next = j
				break
			}
		}
		seen := map[string]bool{}
		for j := i + 1; j < next; j++ {
			if msgs[j].Role == "tool" && msgs[j].ToolCallID != "" {
				seen[msgs[j].ToolCallID] = true
			}
		}
		var missing []string
		for _, tc := range m.ToolCalls {
			if !seen[tc.ID] {
				missing = append(missing, tc.ID)
			}
		}
		if len(missing) == 0 {
			continue
		}
		if next == len(msgs) {
			// 尾部未闭合：在末尾补 tool 消息。
			for _, id := range missing {
				msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: id, Content: InterruptedToolNote})
				rep.ClosedToolCalls++
			}
			rep.Repaired = true
			return msgs, true
		}
		// 中途未闭合：截到该条 assistant 之前，丢弃其后全部消息。
		rep.DroppedMessages = len(msgs) - i
		rep.Repaired = true
		return msgs[:i], true
	}
	return msgs, false
}

// UnclosedToolCallIDs 供测试用：返回历史里最后一条 assistant 未闭合的
// tool_call_id（无则返回空）。
func UnclosedToolCallIDs(msgs []llm.Message) []string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0 {
			seen := map[string]bool{}
			for j := i + 1; j < len(msgs); j++ {
				if msgs[j].Role == "tool" && msgs[j].ToolCallID != "" {
					seen[msgs[j].ToolCallID] = true
				}
			}
			var missing []string
			for _, tc := range msgs[i].ToolCalls {
				if !seen[tc.ID] {
					missing = append(missing, tc.ID)
				}
			}
			return missing
		}
	}
	return nil
}
