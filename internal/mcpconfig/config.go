package mcpconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Server is a configured MCP server (stdio or remote URL).
// Env supports secrets like CONTEXT7_API_KEY / API keys for authenticated MCPs.
type Server struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Enabled     bool              `json:"enabled"`
	Type        string            `json:"type"` // local | remote
	Command     []string          `json:"command,omitempty"`
	URL         string            `json:"url,omitempty"`
	Environment map[string]string `json:"environment,omitempty"` // may hold SK / API keys
	Headers     map[string]string `json:"headers,omitempty"`
	TimeoutMs   int               `json:"timeout_ms,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Public strips secret values for UI listing when mask=true.
func (s Server) Public(mask bool) map[string]any {
	env := map[string]string{}
	for k, v := range s.Environment {
		if mask && v != "" {
			env[k] = "***"
		} else {
			env[k] = v
		}
	}
	hdr := map[string]string{}
	for k, v := range s.Headers {
		if mask && v != "" && (k == "Authorization" || k == "x-api-key" || k == "X-API-Key") {
			hdr[k] = "***"
		} else {
			hdr[k] = v
		}
	}
	return map[string]any{
		"id": s.ID, "name": s.Name, "enabled": s.Enabled, "type": s.Type,
		"command": s.Command, "url": s.URL,
		"environment": env, "headers": hdr,
		"timeout_ms": s.TimeoutMs,
		"created_at": s.CreatedAt, "updated_at": s.UpdatedAt,
		"has_secrets": len(s.Environment) > 0 || len(s.Headers) > 0,
	}
}

type Store struct {
	mu   sync.RWMutex
	path string
	list []Server
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.list = nil
			return nil
		}
		return err
	}
	var list []Server
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	s.list = list
	return nil
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600) // secrets: restrictive perms
}

func (s *Store) List(mask bool) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.list))
	for _, sv := range s.list {
		out = append(out, sv.Public(mask))
	}
	return out
}

func (s *Store) Get(id string) (Server, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sv := range s.list {
		if sv.ID == id {
			return sv, true
		}
	}
	return Server{}, false
}

func (s *Store) Upsert(sv Server) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sv.ID == "" {
		return fmt.Errorf("id required")
	}
	if sv.Name == "" {
		sv.Name = sv.ID
	}
	if sv.Type == "" {
		if sv.URL != "" {
			sv.Type = "remote"
		} else {
			sv.Type = "local"
		}
	}
	now := time.Now().UTC()
	found := false
	for i, existing := range s.list {
		if existing.ID == sv.ID {
			if sv.CreatedAt.IsZero() {
				sv.CreatedAt = existing.CreatedAt
			}
			// merge secrets: empty string in patch means keep existing
			if sv.Environment != nil && existing.Environment != nil {
				for k, v := range sv.Environment {
					if v == "***" || v == "" {
						if old, ok := existing.Environment[k]; ok {
							sv.Environment[k] = old
						}
					}
				}
			}
			if sv.Headers != nil && existing.Headers != nil {
				for k, v := range sv.Headers {
					if v == "***" || v == "" {
						if old, ok := existing.Headers[k]; ok {
							sv.Headers[k] = old
						}
					}
				}
			}
			sv.UpdatedAt = now
			s.list[i] = sv
			found = true
			break
		}
	}
	if !found {
		sv.CreatedAt = now
		sv.UpdatedAt = now
		s.list = append(s.list, sv)
	}
	return s.save()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.list[:0]
	for _, sv := range s.list {
		if sv.ID != id {
			next = append(next, sv)
		}
	}
	s.list = next
	return s.save()
}

// EnabledServers returns configs ready to launch (with real secrets).
func (s *Store) EnabledServers() []Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Server, 0)
	for _, sv := range s.list {
		if sv.Enabled {
			out = append(out, sv)
		}
	}
	return out
}

// CatalogPrompt describes MCP servers for the model (no secret values).
func (s *Store) CatalogPrompt() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var enabled []Server
	for _, sv := range s.list {
		if sv.Enabled {
			enabled = append(enabled, sv)
		}
	}
	if len(enabled) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Configured MCP servers (call via MCP bridge when available):\n")
	for _, sv := range enabled {
		b.WriteString("- ")
		b.WriteString(sv.ID)
		b.WriteString(" (")
		b.WriteString(sv.Type)
		b.WriteString("): ")
		b.WriteString(sv.Name)
		if len(sv.Environment) > 0 {
			b.WriteString(" [has credentials]")
		}
		b.WriteString("\n")
	}
	return b.String()
}
