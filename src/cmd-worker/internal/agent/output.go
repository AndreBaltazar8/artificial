package agent

import (
	"encoding/json"
	"io"
	"os"
)

// StreamEvent represents a parsed event from Claude's stream-json output.
type StreamEvent struct {
	Type string // "text", "tool_use", "error", "result"
	Text string
}

// ParseResult holds metadata extracted during output parsing.
type ParseResult struct {
	SessionID         string
	CWD               string
	RateLimit         bool
	RateLimitResetsAt int64
	InputTokens       int
	OutputTokens      int
}

// Parse reads from reader (PTY or pipe), writes raw bytes to logWriter (the .tty file),
// and tries to extract JSON metadata from any JSON-looking lines.
func Parse(reader io.Reader, logWriter io.Writer, events chan<- StreamEvent) (ParseResult, error) {
	var result ParseResult
	buf := make([]byte, 32*1024)

	// Accumulate bytes for JSON line detection
	var lineBuf []byte

	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := buf[:n]

			// Write raw bytes to .tty log file
			if logWriter != nil {
				logWriter.Write(chunk)
				if f, ok := logWriter.(*os.File); ok {
					f.Sync()
				}
			}

			// Try to find JSON lines in the chunk (for metadata extraction).
			// This is best-effort — PTY output is mostly ANSI, not JSON.
			lineBuf = append(lineBuf, chunk...)
			for {
				idx := -1
				for i, b := range lineBuf {
					if b == '\n' {
						idx = i
						break
					}
				}
				if idx < 0 {
					// Keep at most 64KB in line buffer to avoid unbounded growth
					if len(lineBuf) > 65536 {
						lineBuf = lineBuf[len(lineBuf)-4096:]
					}
					break
				}
				line := lineBuf[:idx]
				lineBuf = lineBuf[idx+1:]

				// Try to parse as JSON for metadata
				if len(line) > 0 && line[0] == '{' {
					parseJSONLine(line, &result, events)
				}
			}
		}
		if err != nil {
			return result, nil // EOF or error — done
		}
	}
}

func parseJSONLine(line []byte, result *ParseResult, events chan<- StreamEvent) {
	var msg map[string]any
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}

	msgType, _ := msg["type"].(string)

	if sid, ok := msg["session_id"].(string); ok && sid != "" {
		result.SessionID = sid
	}
	if msgType == "system" {
		if cwd, ok := msg["cwd"].(string); ok && cwd != "" {
			result.CWD = cwd
		}
	}

	switch msgType {
	case "assistant":
		if errField, ok := msg["error"].(string); ok && errField == "rate_limit" {
			result.RateLimit = true
		}
		if events == nil {
			return
		}
		message, ok := msg["message"].(map[string]any)
		if !ok {
			return
		}
		content, ok := message["content"].([]any)
		if !ok {
			return
		}
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			switch blockType {
			case "text":
				if text, ok := block["text"].(string); ok {
					events <- StreamEvent{Type: "text", Text: text}
				}
			case "tool_use":
				name, _ := block["name"].(string)
				if name != "" {
					events <- StreamEvent{Type: "tool_use", Text: name}
				}
			}
		}
	case "result":
		if usage, ok := msg["usage"].(map[string]any); ok {
			if v, ok := usage["input_tokens"].(float64); ok {
				result.InputTokens = int(v)
			}
			if v, ok := usage["output_tokens"].(float64); ok {
				result.OutputTokens = int(v)
			}
		}
	case "error":
		if errMsg, ok := msg["error"].(map[string]any); ok {
			if errText, ok := errMsg["message"].(string); ok && events != nil {
				events <- StreamEvent{Type: "error", Text: errText}
			}
		}
	}
}
