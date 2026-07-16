package pricing

import "strings"

// Official-ish public list prices (USD per 1M tokens).
// xAI: docs.x.ai — Grok 4.5: $2 in / $0.50 cached / $6 out.
// Kimi Platform: platform.kimi.ai/docs/pricing — K3 / K2.6 / K2.7 Code.
type Rate struct {
	InputPerM  float64 `json:"input_per_m"`
	CachedPerM float64 `json:"cached_per_m"`
	OutputPerM float64 `json:"output_per_m"`
	Label      string  `json:"label"`
}

var table = map[string]Rate{
	"grok-4.5": {
		InputPerM: 2.00, CachedPerM: 0.50, OutputPerM: 6.00, Label: "Grok 4.5",
	},
	"grok-4.5-build": {
		InputPerM: 2.00, CachedPerM: 0.50, OutputPerM: 6.00, Label: "Grok 4.5",
	},
	"grok-composer-2.5-fast": {
		InputPerM: 2.00, CachedPerM: 0.50, OutputPerM: 6.00, Label: "Composer 2.5 Fast (est.)",
	},

	// Kimi K3 flagship (platform.kimi.ai) — $3 cache-miss in / $0.30 cache-hit / $15 out per 1M
	// Desktop Work wire id kimi-for-coding and k3-agent* route to this family.
	"kimi-k3": {
		InputPerM: 3.00, CachedPerM: 0.30, OutputPerM: 15.00, Label: "Kimi K3",
	},
	"kimi-for-coding": {
		InputPerM: 3.00, CachedPerM: 0.30, OutputPerM: 15.00, Label: "Kimi For Coding (K3 est.)",
	},
	"k3-agent": {
		InputPerM: 3.00, CachedPerM: 0.30, OutputPerM: 15.00, Label: "K3 Max / Work (K3 rates)",
	},
	"k3-agent-swarm": {
		InputPerM: 3.00, CachedPerM: 0.30, OutputPerM: 15.00, Label: "K3 Swarm Max (K3 rates)",
	},
	"k3-agent-ultra": {
		InputPerM: 3.00, CachedPerM: 0.30, OutputPerM: 15.00, Label: "K3 Swarm Max (K3 rates)",
	},

	// Kimi K2.6 — $0.95 miss / $0.16 hit / $4 out
	"kimi-k2.6": {
		InputPerM: 0.95, CachedPerM: 0.16, OutputPerM: 4.00, Label: "Kimi K2.6",
	},
	"k2d6-agent": {
		InputPerM: 0.95, CachedPerM: 0.16, OutputPerM: 4.00, Label: "K2.6 Agent (K2.6 rates)",
	},
	"k2p6": {
		InputPerM: 0.95, CachedPerM: 0.16, OutputPerM: 4.00, Label: "K2.6 (est.)",
	},
	"k2p6-agent": {
		InputPerM: 0.95, CachedPerM: 0.16, OutputPerM: 4.00, Label: "K2.6 Agent (est.)",
	},

	// Kimi K2.7 Code — $0.95 miss / $0.19 hit / $4 out
	"kimi-k2.7-code": {
		InputPerM: 0.95, CachedPerM: 0.19, OutputPerM: 4.00, Label: "Kimi K2.7 Code",
	},
	"kimi-k2.7-code-highspeed": {
		InputPerM: 1.90, CachedPerM: 0.38, OutputPerM: 8.00, Label: "Kimi K2.7 Code HighSpeed",
	},
}

// Default when unknown model
var fallback = Rate{InputPerM: 2.00, CachedPerM: 0.50, OutputPerM: 6.00, Label: "Default (Grok 4.5 rates)"}

func NormalizeModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimSuffix(m, "-responses")
	m = strings.TrimSuffix(m, "@responses")
	m = strings.TrimSuffix(m, "/responses")
	m = strings.TrimSuffix(m, "-chat")
	return m
}

func RateFor(model string) Rate {
	m := NormalizeModel(model)
	if r, ok := table[m]; ok {
		return r
	}
	// Kimi Work / Desktop aliases
	if strings.Contains(m, "kimi-for-coding") || strings.HasPrefix(m, "k3-agent") ||
		m == "k3-max" || m == "k3-swarm" || m == "kimi-k3" || m == "k3" {
		return table["kimi-k3"]
	}
	if strings.Contains(m, "k2d6") || strings.Contains(m, "k2p6") || strings.Contains(m, "k2.6") {
		return table["kimi-k2.6"]
	}
	if strings.Contains(m, "k2.7-code") || strings.Contains(m, "k2.7") {
		return table["kimi-k2.7-code"]
	}
	// prefix match
	for k, r := range table {
		if strings.HasPrefix(m, k) || strings.Contains(m, k) {
			return r
		}
	}
	return fallback
}

// CostUSD estimates billable cost.
// Reasoning tokens are billed as output (xAI + Kimi thinking).
func CostUSD(model string, prompt, completion, reasoning, cached int64) float64 {
	r := RateFor(model)
	in := float64(prompt-cached) * r.InputPerM / 1_000_000
	if in < 0 {
		in = float64(prompt) * r.InputPerM / 1_000_000
	}
	cache := float64(cached) * r.CachedPerM / 1_000_000
	outTokens := completion + reasoning
	out := float64(outTokens) * r.OutputPerM / 1_000_000
	return in + cache + out
}

func AllRates() map[string]Rate {
	out := map[string]Rate{}
	for k, v := range table {
		out[k] = v
	}
	return out
}
