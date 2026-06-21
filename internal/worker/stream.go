package worker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	KindThinking   = "thinking"
	KindText       = "text"
	KindTool       = "tool"
	KindToolResult = "tool_result"
	KindResult     = "result"
	KindError      = "error"
	KindSession    = "session"

	lineLimit = 300
)

// Event is one line of activity from a claude -p stream-json run, flattened
// into something a human can read in a log view.
type Event struct {
	Kind       string
	Tool       string  // for KindTool
	Text       string
	CostUSD    float64 // for KindResult
	Turns      int     // for KindResult
	DurationMS int64   // for KindResult
	Usage      Usage   // for KindResult
	SessionID  string  // for KindSession
	Size       int     // for KindToolResult: char count of the tool result content
}

// Usage is the token breakdown from a result event.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens"`
	CacheWriteTokens int `json:"cache_creation_input_tokens"`
}

type streamMessage struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Message   *assistantMsg   `json:"message"`
	Result    json.RawMessage `json:"result"`
	CostUSD   *float64        `json:"total_cost_usd"`
	Duration  *int64          `json:"duration_ms"`
	NumTurns  *int            `json:"num_turns"`
	Usage     *Usage          `json:"usage"`
	Error     json.RawMessage `json:"error"`
}

type assistantMsg struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Thinking  string          `json:"thinking"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result: string or []content block
}

// ParseStream reads claude --output-format stream-json lines from r and
// calls emit for each event. Lines that fail to decode are passed through
// as text so nothing is silently dropped.
func ParseStream(r io.Reader, emit func(Event)) {
	const bufSize = 1024 * 1024
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, bufSize), bufSize)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg streamMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			emit(Event{Kind: KindText, Text: line})
			continue
		}
		switch msg.Type {
		case "system":
			// The init system message is the first line of a run and is the
			// only reliable place to read the session id. Capturing it here
			// lets the worker persist it before the run finishes, so a crash
			// mid-run is resumable. It is also the signal a `--resume`
			// actually loaded the conversation: a resume that fails to find
			// the session emits no init at all (just an error and a result
			// carrying a brand-new throwaway session id), so the runner must
			// not treat the result's session id as "resume succeeded" — only
			// this init event counts.
			if msg.Subtype == "init" && msg.SessionID != "" {
				emit(Event{Kind: KindSession, SessionID: msg.SessionID})
			}
		case "assistant":
			emitAssistant(msg.Message, emit)
		case "user":
			emitUser(msg.Message, emit)
		case "result":
			emit(resultEvent(msg))
			if msg.Subtype == "error_max_turns" {
				emit(Event{Kind: KindError, Text: "hit max turns"})
			}
		case "error":
			var s string
			if json.Unmarshal(msg.Error, &s) != nil {
				s = string(msg.Error)
			}
			emit(Event{Kind: KindError, Text: s})
		}
	}
}

func emitAssistant(m *assistantMsg, emit func(Event)) {
	if m == nil {
		return
	}
	for _, b := range m.Content {
		switch b.Type {
		case "thinking":
			if b.Thinking != "" {
				emit(Event{Kind: KindThinking, Text: b.Thinking})
			}
		case "text":
			if b.Text != "" {
				emit(Event{Kind: KindText, Text: b.Text})
			}
		case "tool_use":
			emit(Event{Kind: KindTool, Tool: b.Name, Text: summariseInput(b.Name, b.Input), Size: len(b.Input)})
		}
	}
}

func emitUser(m *assistantMsg, emit func(Event)) {
	if m == nil {
		return
	}
	for _, b := range m.Content {
		if b.Type == "tool_result" {
			n := toolResultChars(b.Content)
			emit(Event{Kind: KindToolResult, Size: n, Text: formatChars(n)})
		}
	}
}

// toolResultChars returns the character count of a tool result's content field,
// which can be either a JSON string or an array of text content blocks.
func toolResultChars(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return len(s)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		n := 0
		for _, b := range blocks {
			n += len(b.Text)
		}
		return n
	}
	return len(raw)
}

// formatChars renders a character count as "842 chars" or "12.1k chars".
func formatChars(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk chars", float64(n)/1000)
	}
	return fmt.Sprintf("%d chars", n)
}

func resultEvent(msg streamMessage) Event {
	ev := Event{Kind: KindResult}
	if len(msg.Result) > 0 {
		var s string
		if json.Unmarshal(msg.Result, &s) == nil {
			ev.Text = s
		} else {
			ev.Text = string(msg.Result)
		}
	}
	if msg.CostUSD != nil {
		ev.CostUSD = *msg.CostUSD
	}
	if msg.NumTurns != nil {
		ev.Turns = *msg.NumTurns
	}
	if msg.Duration != nil {
		ev.DurationMS = *msg.Duration
	}
	if msg.Usage != nil {
		ev.Usage = *msg.Usage
	}
	return ev
}

func summariseInput(tool string, raw json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	switch tool {
	case "Bash":
		if c, _ := m["command"].(string); c != "" {
			return c
		}
	case "Read", "Write", "Edit":
		if p, _ := m["file_path"].(string); p != "" {
			return p
		}
	case "Grep", "Glob":
		if p, _ := m["pattern"].(string); p != "" {
			return p
		}
	}
	if len(raw) > 0 {
		return truncate(string(raw))
	}
	return ""
}

func truncate(s string) string {
	if len(s) <= lineLimit {
		return s
	}
	return s[:lineLimit] + fmt.Sprintf("… (%d chars)", len(s))
}

// FormatEvent renders an Event as one log line.
func FormatEvent(e Event) string {
	switch e.Kind {
	case KindThinking:
		return "[thinking] " + truncate(e.Text)
	case KindTool:
		return fmt.Sprintf("[%s] %s", strings.ToLower(e.Tool), truncate(e.Text))
	case KindToolResult:
		return "[tool-result] " + e.Text
	case KindResult:
		var b strings.Builder
		fmt.Fprintf(&b, "[result] cost=$%.4f turns=%d", e.CostUSD, e.Turns)
		if e.DurationMS > 0 {
			fmt.Fprintf(&b, " dur=%ds", e.DurationMS/1000)
		}
		u := e.Usage
		if u.InputTokens > 0 || u.OutputTokens > 0 {
			fmt.Fprintf(&b, " in=%d out=%d", u.InputTokens, u.OutputTokens)
		}
		if u.CacheReadTokens > 0 {
			fmt.Fprintf(&b, " cache_read=%d", u.CacheReadTokens)
		}
		if u.CacheWriteTokens > 0 {
			fmt.Fprintf(&b, " cache_write=%d", u.CacheWriteTokens)
		}
		if ctx := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens; ctx > 0 {
			fmt.Fprintf(&b, " ctx=%d", ctx)
		}
		if t := truncate(e.Text); t != "" {
			fmt.Fprintf(&b, " %s", t)
		}
		return b.String()
	case KindSession:
		return "[session] " + e.SessionID
	case KindError:
		return "[error] " + e.Text
	default:
		return e.Text
	}
}
