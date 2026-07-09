package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Domain  string `json:"domain"`
	Source  string `json:"source"` // duckduckgo | instant
	Icon    string `json:"icon"`   // favicon url
}

type Response struct {
	Query      string   `json:"query"`
	Results    []Result `json:"results"`
	Abstract   string   `json:"abstract,omitempty"`
	Answer     string   `json:"answer,omitempty"`
	AnswerURL  string   `json:"answer_url,omitempty"`
	Provider   string   `json:"provider"`
	DurationMs int64    `json:"duration_ms"`
}

type Client struct {
	HTTP *http.Client
}

func New() *Client {
	return &Client{
		HTTP: &http.Client{Timeout: 18 * time.Second},
	}
}

func (c *Client) Search(ctx context.Context, query string, limit int) (*Response, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("empty query")
	}
	if limit <= 0 {
		limit = 6
	}
	if limit > 12 {
		limit = 12
	}

	t0 := time.Now()
	out := &Response{
		Query:    q,
		Provider: "DuckDuckGo",
		Results:  []Result{},
	}

	// 1) Instant Answer (abstract / answer box)
	if abs, err := c.instantAnswer(ctx, q); err == nil && abs != nil {
		out.Abstract = abs.Abstract
		out.Answer = abs.Answer
		out.AnswerURL = abs.AbstractURL
		for _, r := range abs.Related {
			if len(out.Results) >= limit {
				break
			}
			out.Results = append(out.Results, r)
		}
	}

	// 2) HTML organic results (richer web hits)
	organic, err := c.htmlSearch(ctx, q, limit)
	if err == nil {
		// merge, prefer organic first
		merged := make([]Result, 0, limit)
		seen := map[string]bool{}
		for _, r := range organic {
			k := strings.ToLower(r.URL)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			merged = append(merged, r)
		}
		for _, r := range out.Results {
			k := strings.ToLower(r.URL)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			merged = append(merged, r)
		}
		if len(merged) > limit {
			merged = merged[:limit]
		}
		out.Results = merged
	}

	// Favicons
	for i := range out.Results {
		if out.Results[i].Domain == "" {
			out.Results[i].Domain = domainOf(out.Results[i].URL)
		}
		if out.Results[i].Icon == "" && out.Results[i].Domain != "" {
			out.Results[i].Icon = "https://www.google.com/s2/favicons?domain=" + url.QueryEscape(out.Results[i].Domain) + "&sz=32"
		}
	}

	out.DurationMs = time.Since(t0).Milliseconds()
	if len(out.Results) == 0 && out.Abstract == "" && out.Answer == "" {
		return out, fmt.Errorf("no results for %q", q)
	}
	return out, nil
}

type instantPayload struct {
	Abstract    string
	AbstractURL string
	Answer      string
	Related     []Result
}

func (c *Client) instantAnswer(ctx context.Context, q string) (*instantPayload, error) {
	u := "https://api.duckduckgo.com/?" + url.Values{
		"q":            {q},
		"format":       {"json"},
		"no_html":      {"1"},
		"skip_disambig": {"1"},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "GrokProxyPlus/1.0 (+https://github.com/Maicon501a/grok-proxy-plus)")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("instant answer HTTP %d", resp.StatusCode)
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return nil, fmt.Errorf("instant answer json")
	}
	out := &instantPayload{}
	if s, ok := raw["AbstractText"].(string); ok {
		out.Abstract = strings.TrimSpace(s)
	}
	if s, ok := raw["AbstractURL"].(string); ok {
		out.AbstractURL = strings.TrimSpace(s)
	}
	if s, ok := raw["Answer"].(string); ok {
		out.Answer = strings.TrimSpace(s)
	}
	// RelatedTopics
	if arr, ok := raw["RelatedTopics"].([]any); ok {
		out.Related = flattenRelated(arr, 8)
	}
	// Results array (sometimes present)
	if arr, ok := raw["Results"].([]any); ok {
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			r := Result{
				Title:   str(m["Text"]),
				URL:     str(m["FirstURL"]),
				Snippet: str(m["Text"]),
				Source:  "instant",
			}
			r.Domain = domainOf(r.URL)
			if r.URL != "" {
				out.Related = append(out.Related, r)
			}
		}
	}
	return out, nil
}

func flattenRelated(arr []any, limit int) []Result {
	var out []Result
	var walk func([]any)
	walk = func(items []any) {
		for _, item := range items {
			if len(out) >= limit {
				return
			}
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if topics, ok := m["Topics"].([]any); ok {
				walk(topics)
				continue
			}
			text := str(m["Text"])
			first := str(m["FirstURL"])
			if first == "" {
				continue
			}
			out = append(out, Result{
				Title:   firstLine(text),
				URL:     first,
				Snippet: text,
				Domain:  domainOf(first),
				Source:  "instant",
			})
		}
	}
	walk(arr)
	return out
}

func (c *Client) htmlSearch(ctx context.Context, q string, limit int) ([]Result, error) {
	// lite endpoint is friendlier for scraping
	form := url.Values{"q": {q}}
	u := "https://html.duckduckgo.com/html/?" + form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GrokProxyPlus/1.0; +https://github.com/Maicon501a/grok-proxy-plus)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,pt-BR;q=0.8")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ddg html HTTP %d", resp.StatusCode)
	}
	doc, err := html.Parse(io.LimitReader(resp.Body, 3<<20))
	if err != nil {
		return nil, err
	}

	var results []Result
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(results) >= limit {
			return
		}
		if n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "result") {
			r := parseResultDiv(n)
			if r.URL != "" && r.Title != "" {
				r.Source = "duckduckgo"
				r.Domain = domainOf(r.URL)
				results = append(results, r)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	// Fallback: any result__a links
	if len(results) == 0 {
		results = parseResultLinks(doc, limit)
	}
	return results, nil
}

func parseResultDiv(n *html.Node) Result {
	var r Result
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.ElementNode {
			if x.Data == "a" && hasClass(x, "result__a") {
				r.URL = absAttr(x, "href")
				r.Title = strings.TrimSpace(textContent(x))
			}
			if (x.Data == "a" || x.Data == "td") && hasClass(x, "result__snippet") {
				r.Snippet = strings.TrimSpace(textContent(x))
			}
			if x.Data == "a" && hasClass(x, "result__url") {
				if r.Domain == "" {
					r.Domain = strings.TrimSpace(textContent(x))
				}
			}
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	// DDG sometimes wraps redirects
	r.URL = unwrapDDG(r.URL)
	return r
}

func parseResultLinks(doc *html.Node, limit int) []Result {
	var out []Result
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(out) >= limit {
			return
		}
		if n.Type == html.ElementNode && n.Data == "a" && hasClass(n, "result__a") {
			u := unwrapDDG(absAttr(n, "href"))
			t := strings.TrimSpace(textContent(n))
			if u != "" && t != "" {
				out = append(out, Result{
					Title:  t,
					URL:    u,
					Domain: domainOf(u),
					Source: "duckduckgo",
				})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

var uddgRe = regexp.MustCompile(`uddg=([^&]+)`)

func unwrapDDG(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	// //duckduckgo.com/l/?uddg=...
	if strings.Contains(href, "uddg=") {
		if m := uddgRe.FindStringSubmatch(href); len(m) == 2 {
			if dec, err := url.QueryUnescape(m[1]); err == nil {
				return dec
			}
		}
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	return href
}

func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if c == class {
					return true
				}
			}
		}
	}
	return false
}

func absAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func textContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(textContent(c))
	}
	return b.String()
}

func domainOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(u.Host, "www.")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n-"); i > 0 && i < 80 {
		return strings.TrimSpace(s[:i])
	}
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// FormatForPrompt builds a compact context block for the model.
func FormatForPrompt(res *Response) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Web search results via DuckDuckGo for query: " + res.Query + "\n")
	if res.Answer != "" {
		b.WriteString("Direct answer: " + res.Answer + "\n")
	}
	if res.Abstract != "" {
		b.WriteString("Abstract: " + res.Abstract + "\n")
		if res.AnswerURL != "" {
			b.WriteString("Source: " + res.AnswerURL + "\n")
		}
	}
	b.WriteString("\nResults:\n")
	for i, r := range res.Results {
		b.WriteString(fmt.Sprintf("%d. %s\n   URL: %s\n", i+1, r.Title, r.URL))
		if r.Snippet != "" {
			b.WriteString("   " + trimSnippet(r.Snippet, 280) + "\n")
		}
	}
	b.WriteString("\nUse these sources when answering. Cite URLs when relevant. If results are insufficient, say so.\n")
	return b.String()
}

func trimSnippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
