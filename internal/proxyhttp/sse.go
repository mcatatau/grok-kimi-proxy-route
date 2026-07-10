package proxyhttp

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// flushWriter wraps ResponseWriter and flushes after every write when possible.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newFlushWriter(w http.ResponseWriter) *flushWriter {
	f, _ := w.(http.Flusher)
	return &flushWriter{w: w, f: f}
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if f.f != nil {
		f.f.Flush()
	}
	return n, err
}

func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

func writeSSE(w io.Writer, event string, data string) error {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteString("\n")
	}
	// multi-line data support
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func writeSSEJSON(w io.Writer, event string, payload any, marshal func(any) ([]byte, error)) error {
	b, err := marshal(payload)
	if err != nil {
		return err
	}
	return writeSSE(w, event, string(b))
}

// pipeSSE copies an upstream SSE body to client, flushing each event.
func pipeSSE(dst http.ResponseWriter, src io.Reader) error {
	pipeSSEWithUsage(dst, src, "")
	return nil
}

type sseUsageCapture struct {
	promptTokens     int64
	completionTokens int64
	reasoningTokens  int64
	totalTokens      int64
}

// pipeSSEWithUsage copies SSE and extracts usage data from the stream.
// Returns usage info parsed from Chat Completions or Responses format.
func pipeSSEWithUsage(dst http.ResponseWriter, src io.Reader, path string) sseUsageCapture {
	setSSEHeaders(dst)
	dst.WriteHeader(http.StatusOK)
	fw := newFlushWriter(dst)
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64*1024), 2<<20)
	var block strings.Builder
	var usage sseUsageCapture
	lastEvent := ""

	flushBlock := func() error {
		if block.Len() == 0 {
			return nil
		}
		if strings.HasSuffix(block.String(), "\n\n") {
			// check data lines for usage
			lines := strings.Split(block.String(), "\n")
			for _, bl := range lines {
				bl = strings.TrimSpace(bl)
				if strings.HasPrefix(bl, "data:") {
					payload := strings.TrimSpace(bl[5:])
					if strings.HasPrefix(payload, "{") {
						extracted := extractUsageFromPayload(payload, lastEvent, path)
						if extracted.promptTokens > 0 || extracted.completionTokens > 0 || extracted.totalTokens > 0 {
							usage = extracted
						}
					}
				}
				if strings.HasPrefix(bl, "event:") {
					lastEvent = strings.TrimSpace(bl[6:])
				}
			}
		}
		_, err := io.WriteString(fw, block.String())
		if !strings.HasSuffix(block.String(), "\n\n") {
			_, _ = io.WriteString(fw, "\n")
		}
		block.Reset()
		return err
	}
	for sc.Scan() {
		line := sc.Text()
		block.WriteString(line)
		block.WriteByte('\n')
		if line == "" {
			if err := flushBlock(); err != nil {
				log.Printf("proxyhttp sse flush: %v", err)
				return usage
			}
		}
	}
	_ = flushBlock()
	if err := sc.Err(); err != nil {
		log.Printf("proxyhttp sse scan: %v", err)
	}
	return usage
}

func extractUsageFromPayload(payload, lastEvent, path string) sseUsageCapture {
	var u sseUsageCapture
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return u
	}
	// Chat completions: {"usage": {"prompt_tokens":..., "completion_tokens":...}}
	if usage, ok := obj["usage"].(map[string]any); ok {
		u.promptTokens = asInt64(usage["prompt_tokens"])
		u.completionTokens = asInt64(usage["completion_tokens"])
		u.totalTokens = asInt64(usage["total_tokens"])
		if d, ok := usage["completion_tokens_details"].(map[string]any); ok {
			u.reasoningTokens = asInt64(d["reasoning_tokens"])
		}
	}
	// Responses format: event=response.completed, data={"response":{"usage":{...}}}
	if lastEvent == "response.completed" || lastEvent == "done" {
		if r, ok := obj["response"].(map[string]any); ok {
			if usage, ok := r["usage"].(map[string]any); ok {
				u.promptTokens = asInt64(usage["input_tokens"])
				u.completionTokens = asInt64(usage["output_tokens"])
				u.totalTokens = asInt64(usage["total_tokens"])
				if d, ok := usage["output_tokens_details"].(map[string]any); ok {
					u.reasoningTokens = asInt64(d["reasoning_tokens"])
				}
			}
		}
	}
	return u
}
