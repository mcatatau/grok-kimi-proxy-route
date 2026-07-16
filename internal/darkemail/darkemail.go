package darkemail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const DefaultBase = "https://www.darkemail.school"

var (
	reOTP6     = regexp.MustCompile(`(?i)(?:^|[^0-9])([0-9]{6})(?:[^0-9]|$)`)
	reCodeDash = regexp.MustCompile(`(?i)\b([A-Z0-9]{2,5}-[A-Z0-9]{2,5})\b`)
	reSubjLead = regexp.MustCompile(`(?i)^([A-Z0-9]{2,5}-[A-Z0-9]{2,5})\b`)
	reLabeled6 = regexp.MustCompile(`(?i)(?:code|otp|verification)[^0-9]{0,40}([0-9]{6})\b`)
	adjs       = []string{"swift", "brave", "calm", "keen", "witty", "bright", "cute", "bold", "wise", "fast", "cool", "fair", "gold", "wild"}
	nouns      = []string{"panda", "tiger", "eagle", "wolf", "bear", "lion", "hawk", "deer", "kiwi", "lark", "orca", "puma", "seal", "zebra", "koala", "turtle"}
	reLink     = regexp.MustCompile(`https?://[^\s"'<>]+`)
)

type Client struct {
	Base   string
	HTTP   *http.Client
	Domain string
}

type Message struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"`
	BodyHTML  string `json:"bodyHtml"`
	BodyText  string `json:"bodyText"`
	Body      string `json:"body"`
	From      string `json:"from"`
	Date      string `json:"date"`
	Timestamp any    `json:"timestamp"`
}

type listResponse struct {
	Emails      []Message `json:"emails"`
	Count       int       `json:"count"`
	Recipient   string    `json:"recipient"`
	CachedCount int       `json:"cachedCount"`
}

func New() *Client {
	return &Client{
		Base:   DefaultBase,
		Domain: "darkemail.school",
		HTTP:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *Client) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return DefaultBase
}

func (c *Client) domain() string {
	if c.Domain != "" {
		return c.Domain
	}
	return "darkemail.school"
}

// GenEmail builds a disposable address. No API create is required —
// any local-part under the domain is a valid inbox.
func (c *Client) GenEmail() string {
	adj := adjs[rand.Intn(len(adjs))]
	noun := nouns[rand.Intn(len(nouns))]
	return fmt.Sprintf("%s-%s%d-%s@%s", adj, noun, rand.Intn(900)+100, randStr(8), c.domain())
}

func (c *Client) Probe(ctx context.Context) error {
	_, err := c.List(ctx, "probe@"+c.domain())
	return err
}

func (c *Client) List(ctx context.Context, email string) ([]Message, error) {
	u := fmt.Sprintf("%s/api/emails?to=%s", c.base(), url.QueryEscape(email))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grok-desktop-signup/0.1")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("darkemail HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var out listResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	// normalize bodyHtml from body field when needed
	for i := range out.Emails {
		if out.Emails[i].BodyHTML == "" && out.Emails[i].Body != "" {
			out.Emails[i].BodyHTML = out.Emails[i].Body
		}
	}
	return out.Emails, nil
}

// WaitForMessage polls until a message with a parseable OTP appears or timeout.
func (c *Client) WaitForMessage(ctx context.Context, email string, timeout time.Duration) (*Message, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	var last *Message
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		msgs, err := c.List(ctx, email)
		if err != nil {
			lastErr = err
			time.Sleep(3 * time.Second)
			continue
		}
		if len(msgs) > 0 {
			m := msgs[0]
			last = &m
			if ExtractOTP(&m) != "" {
				return &m, nil
			}
		}
		time.Sleep(4 * time.Second)
	}
	if last != nil {
		return last, fmt.Errorf("mail arrived but OTP not parsed subject=%q", last.Subject)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("timeout waiting mail: %w", lastErr)
	}
	return nil, fmt.Errorf("timeout waiting mail for %s", email)
}

// ExtractOTP prefers xAI-style dashed codes ("W63-TN8"), then labeled 6-digit OTPs.
func ExtractOTP(m *Message) string {
	if m == nil {
		return ""
	}
	html := m.BodyHTML
	if html == "" {
		html = m.Body
	}
	text := stripTags(html)
	if sm := reSubjLead.FindStringSubmatch(strings.TrimSpace(m.Subject)); len(sm) > 1 {
		return strings.ToUpper(sm[1])
	}
	blob := m.Subject + "\n" + text + "\n" + m.BodyText
	if sm := reCodeDash.FindStringSubmatch(blob); len(sm) > 1 {
		return strings.ToUpper(sm[1])
	}
	if sm := reLabeled6.FindStringSubmatch(blob); len(sm) > 1 {
		return sm[1]
	}
	if sm := reOTP6.FindStringSubmatch(blob); len(sm) > 1 {
		code := sm[1]
		switch code {
		case "000000", "123456", "333333", "111111":
		default:
			return code
		}
	}
	return ""
}

func ExtractLinks(m *Message) []string {
	if m == nil {
		return nil
	}
	html := m.BodyHTML
	if html == "" {
		html = m.Body
	}
	blob := html + "\n" + m.BodyText + "\n" + m.Subject
	raw := reLink.FindAllString(blob, -1)
	seen := map[string]bool{}
	var out []string
	for _, l := range raw {
		l = strings.TrimRight(l, ".,);]\">'")
		l = strings.ReplaceAll(l, "&amp;", "&")
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

func stripTags(s string) string {
	reStyle := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reScript := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reTag := regexp.MustCompile(`<[^>]+>`)
	s = reStyle.ReplaceAllString(s, " ")
	s = reScript.ReplaceAllString(s, " ")
	s = reTag.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	return strings.Join(strings.Fields(s), " ")
}

func randStr(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
