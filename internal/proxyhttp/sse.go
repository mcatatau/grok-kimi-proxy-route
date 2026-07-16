package proxyhttp

import (
	"bufio"
	"fmt"
	"io"
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
	// Client disconnect mid-stream is normal (Codex cancel / app focus). Never panic.
	defer func() { _ = recover() }()
	n, err := f.w.Write(p)
	if err == nil && f.f != nil {
		func() {
			defer func() { _ = recover() }()
			f.f.Flush()
		}()
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
// Uses a large scanner limit so fat Grok tool/reasoning frames do not abort the stream.
func pipeSSE(dst http.ResponseWriter, src io.Reader) error {
	setSSEHeaders(dst)
	dst.WriteHeader(http.StatusOK)
	fw := newFlushWriter(dst)
	// 16MiB max token — Grok/Codex can emit very large single SSE data lines.
	const maxLine = 16 << 20
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 256*1024), maxLine)
	var block strings.Builder
	flushBlock := func() error {
		if block.Len() == 0 {
			return nil
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
				// Client gone — stop quietly; do not surface as process death.
				return nil
			}
		}
	}
	_ = flushBlock()
	if err := sc.Err(); err != nil {
		// Token too long / network blip: log-friendly error, no panic.
		return fmt.Errorf("sse scan: %w", err)
	}
	return nil
}
