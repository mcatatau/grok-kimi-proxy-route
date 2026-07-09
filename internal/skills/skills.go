package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Skill is a local markdown skill (compatible with common SKILL.md layouts).
type Skill struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Path        string    `json:"path"`
	Body        string    `json:"body,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Store struct {
	root string
}

func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) List() ([]Skill, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sk, err := s.Get(e.Name())
		if err != nil {
			continue
		}
		sk.Body = "" // list without body
		out = append(out, *sk)
	}
	return out, nil
}

func (s *Store) Get(id string) (*Skill, error) {
	id = sanitizeID(id)
	dir := filepath.Join(s.root, id)
	// prefer SKILL.md then skill.md then README.md
	var path string
	for _, name := range []string{"SKILL.md", "skill.md", "README.md"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
	}
	if path == "" {
		return nil, fmt.Errorf("skill not found: %s", id)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	name, desc, body := parseFrontmatter(string(b))
	if name == "" {
		name = id
	}
	st, _ := os.Stat(path)
	updated := time.Now()
	if st != nil {
		updated = st.ModTime()
	}
	return &Skill{
		ID:          id,
		Name:        name,
		Description: desc,
		Path:        path,
		Body:        body,
		UpdatedAt:   updated,
	}, nil
}

func (s *Store) Create(name, description, body string) (*Skill, error) {
	id := sanitizeID(name)
	if id == "" {
		return nil, fmt.Errorf("invalid skill name")
	}
	dir := filepath.Join(s.root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body) == "" {
		body = "# " + name + "\n\n" + description + "\n"
	}
	content := formatSkillMD(name, description, body)
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *Store) Update(id, name, description, body string) (*Skill, error) {
	id = sanitizeID(id)
	dir := filepath.Join(s.root, id)
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("skill not found")
	}
	if name == "" {
		name = id
	}
	content := formatSkillMD(name, description, body)
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *Store) Delete(id string) error {
	id = sanitizeID(id)
	dir := filepath.Join(s.root, id)
	return os.RemoveAll(dir)
}

// CatalogPrompt builds a compact system prompt block listing skills for the model.
func (s *Store) CatalogPrompt() string {
	list, err := s.List()
	if err != nil || len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available local skills (load full body only when needed by id):\n")
	for _, sk := range list {
		b.WriteString("- ")
		b.WriteString(sk.ID)
		b.WriteString(": ")
		b.WriteString(sk.Description)
		if sk.Description == "" {
			b.WriteString(sk.Name)
		}
		b.WriteString("\n")
	}
	b.WriteString("You can create a new skill when the user asks (use create_skill tool).\n")
	return b.String()
}

func formatSkillMD(name, desc, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: ")
	b.WriteString(strings.TrimSpace(name))
	b.WriteString("\n")
	if strings.TrimSpace(desc) != "" {
		b.WriteString("description: ")
		b.WriteString(strings.TrimSpace(desc))
		b.WriteString("\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
	return b.String()
}

var fmRe = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?(.*)$`)

func parseFrontmatter(raw string) (name, desc, body string) {
	m := fmRe.FindStringSubmatch(raw)
	if m == nil {
		return "", "", raw
	}
	meta, rest := m[1], m[2]
	for _, line := range strings.Split(meta, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		}
		if strings.HasPrefix(line, "description:") {
			desc = strings.TrimSpace(strings.TrimPrefix(line, "description:"))
		}
	}
	return name, desc, rest
}

func sanitizeID(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, " ", "-")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
