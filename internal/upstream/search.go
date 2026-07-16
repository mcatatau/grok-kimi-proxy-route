package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"grok-desktop/internal/store"
)

// SearchResponse is the aggregated native search output from xAI Responses tools.
type SearchResponse struct {
	ID      string
	Model   string
	Answer  string
	Results []map[string]any
	Usage   Usage
}

// NativeSearch runs one Responses turn with only web_search / x_search tools and
// returns structured results. Auth is the same bearer token used by the proxy.
func (c *Client) NativeSearch(
	ctx context.Context,
	token string,
	settings store.Settings,
	model, effort, query string,
	sources []string,
) (*SearchResponse, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = settings.DefaultModel
	}
	if effort == "" {
		effort = "low"
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty query")
	}
	if len(sources) == 0 {
		sources = []string{"web", "x"}
	}
	tools := make([]map[string]any, 0, 2)
	for _, s := range sources {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "web", "web_search":
			tools = append(tools, map[string]any{"type": "web_search"})
		case "x", "x_search", "twitter":
			tools = append(tools, map[string]any{"type": "x_search"})
		}
	}
	if len(tools) == 0 {
		tools = []map[string]any{{"type": "web_search"}, {"type": "x_search"}}
	}

	input := []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": "Search the following query using your tools. Prefer returning sources. Query: " + query,
				},
			},
		},
	}

	body := map[string]any{
		"model":  model,
		"input":  input,
		"stream": true,
		"reasoning": map[string]any{
			"effort":  effort,
			"summary": "auto",
		},
		"tools":       tools,
		"tool_choice": "auto",
		"store":       false,
	}
	raw, _ := json.Marshal(body)
	url := c.baseURL(settings) + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header = c.authHeaders(token, settings.ClientVersion, settings)
	httpReq.Header.Set("Accept", "text/event-stream, application/json")

	// /v1/search aggregates until completion. Some upstream SSE connections stay
	// open after response.completed, so waiting for EOF hangs clients forever.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		httpReq = httpReq.Clone(ctx)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("responses HTTP %d: %s", resp.StatusCode, string(b))
	}

	out := &SearchResponse{Results: []map[string]any{}}
	pending := map[string]map[string]any{}
	var answer strings.Builder
	var eventName string
	done := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			done = true
			break
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		typ, _ := obj["type"].(string)
		if typ == "" {
			typ = eventName
		}
		switch typ {
		case "response.created", "response.in_progress":
			if r, ok := obj["response"].(map[string]any); ok {
				if v, ok := r["id"].(string); ok {
					out.ID = v
				}
				if v, ok := r["model"].(string); ok {
					out.Model = v
				}
			}
		case "response.output_text.delta":
			if d, ok := obj["delta"].(string); ok {
				answer.WriteString(d)
			}
		case "response.output_item.added":
			if item, ok := obj["item"].(map[string]any); ok {
				kind := searchKindFromItem(item)
				if kind == "" {
					break
				}
				itemID := strField(item["id"])
				if itemID == "" {
					itemID = out.ID
				}
				pending[itemID] = map[string]any{"kind": kind, "query": extractSearchQuery(item)}
			}
		case "response.output_item.done":
			if item, ok := obj["item"].(map[string]any); ok {
				mergeSearchItem(out, pending, item, query)
			}
		case "error", "response.failed":
			msg := strField(obj["error"])
			if msg == "" {
				if errObj, ok := obj["error"].(map[string]any); ok {
					msg = firstNonEmpty(strField(errObj["message"]), strField(errObj["code"]))
				}
			}
			if msg == "" {
				msg = strField(obj["message"])
			}
			if msg == "" {
				msg = typ
			}
			return nil, fmt.Errorf("responses %s: %s", typ, msg)
		case "response.completed":
			if r, ok := obj["response"].(map[string]any); ok {
				if v, ok := r["id"].(string); ok {
					out.ID = v
				}
				if v, ok := r["model"].(string); ok {
					out.Model = v
				}
				if u, ok := r["usage"].(map[string]any); ok {
					if usage := parseResponsesUsage(u); usage != nil {
						out.Usage = *usage
					}
				}
				// Fallback harvest if item.done was missed.
				if output, ok := r["output"].([]any); ok {
					for _, raw := range output {
						item, _ := raw.(map[string]any)
						if item == nil {
							continue
						}
						mergeSearchItem(out, pending, item, query)
						if answer.Len() == 0 {
							if content, ok := item["content"].([]any); ok {
								for _, c := range content {
									m, _ := c.(map[string]any)
									if m == nil {
										continue
									}
									if strField(m["type"]) == "output_text" {
										if txt := strField(m["text"]); txt != "" {
											answer.WriteString(txt)
										}
									}
								}
							}
						}
					}
				}
			}
			done = true
		}
		eventName = ""
		if done {
			break
		}
	}
	if err := sc.Err(); err != nil && !done {
		return nil, err
	}
	out.Answer = strings.TrimSpace(answer.String())
	if out.ID == "" {
		out.ID = fmt.Sprintf("search_%d", time.Now().UnixNano())
	}
	if out.Usage.TotalTokens == 0 {
		out.Usage.TotalTokens = out.Usage.PromptTokens + out.Usage.CompletionTokens
	}
	return out, nil
}

func mergeSearchItem(out *SearchResponse, pending map[string]map[string]any, item map[string]any, fallbackQuery string) {
	kind := searchKindFromItem(item)
	if kind == "" {
		return
	}
	itemID := strField(item["id"])
	if itemID == "" {
		itemID = out.ID
	}
	q := extractSearchQuery(item)
	if q == "" {
		if p, ok := pending[itemID]; ok {
			q = strField(p["query"])
		}
	}
	if q == "" {
		q = fallbackQuery
	}
	for _, hit := range extractSearchResults(item) {
		hit["kind"] = kind
		hit["query"] = q
		u := strField(hit["url"])
		dup := false
		for _, existing := range out.Results {
			if strField(existing["url"]) == u && u != "" {
				dup = true
				break
			}
		}
		if !dup {
			out.Results = append(out.Results, hit)
		}
	}
	delete(pending, itemID)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
