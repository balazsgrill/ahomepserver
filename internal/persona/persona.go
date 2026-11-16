package persona

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"gopkg.in/yaml.v3"
)

// LLMConfig defines the LLM endpoint, model, and token key for a persona.
type LLMConfig struct {
	Endpoint    string   `yaml:"endpoint" json:"endpoint"`
	Model       string   `yaml:"model" json:"model"`
	TokenKey    string   `yaml:"token_key" json:"token_key"`
	Temperature *float64 `yaml:"temperature,omitempty" json:"temperature,omitempty"`
	MaxTokens   int      `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
}

// MCPServerConfig defines an external MCP server endpoint for a persona.
type MCPServerConfig struct {
	Name      string            `yaml:"name" json:"name"`
	URL       string            `yaml:"url" json:"url"`
	Transport string            `yaml:"transport,omitempty" json:"transport,omitempty"` // "http" (default) or "sse"
	TokenKey  string            `yaml:"token_key,omitempty" json:"token_key,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Tools     []string          `yaml:"tools,omitempty" json:"tools,omitempty"`
}

// AllowsTool returns true if the MCPServerConfig has no tool filter or if toolName matches an entry (exact or prefix*).
func (m MCPServerConfig) AllowsTool(toolName string) bool {
	if len(m.Tools) == 0 {
		return true
	}
	for _, pattern := range m.Tools {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" || pattern == "*" {
			return true
		}
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(toolName, prefix) {
				return true
			}
		} else if strings.EqualFold(pattern, toolName) {
			return true
		}
	}
	return false
}

// WorkflowTemplate defines an optional named workflow template on a persona.
type WorkflowTemplate struct {
	Name           string `yaml:"name" json:"name"`
	Description    string `yaml:"description,omitempty" json:"description,omitempty"`
	PromptTemplate string `yaml:"prompt_template" json:"prompt_template"`
}

// Skill represents a loaded skill belonging to a persona.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	FilePath    string `json:"file_path"`
}

// Persona represents an agent persona loaded from a folder.
type Persona struct {
	ID           string             `yaml:"id" json:"id"`
	Name         string             `yaml:"name" json:"name"`
	Description  string             `yaml:"description" json:"description"`
	LLM          LLMConfig          `yaml:"llm" json:"llm"`
	MCPServers   []MCPServerConfig  `yaml:"mcp_servers" json:"mcp_servers"`
	Workflows    []WorkflowTemplate `yaml:"workflows,omitempty" json:"workflows,omitempty"`
	Instructions string             `yaml:"instructions,omitempty" json:"instructions"`
	Skills       []Skill            `yaml:"-" json:"skills"`
	DirPath      string             `yaml:"-" json:"dir_path"`
	ContentHash  string             `yaml:"-" json:"content_hash"`
	LoadedAt     time.Time          `yaml:"-" json:"loaded_at"`
}

// SystemPrompt builds the combined system prompt from base instructions and skills overview.
func (p *Persona) SystemPrompt() string {
	var sb strings.Builder
	if strings.TrimSpace(p.Instructions) != "" {
		sb.WriteString(strings.TrimSpace(p.Instructions))
		sb.WriteString("\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("You are %s.", p.Name))
		if p.Description != "" {
			sb.WriteString(" " + p.Description)
		}
		sb.WriteString("\n\n")
	}

	if len(p.Skills) > 0 {
		sb.WriteString("# Available Skills\n")
		sb.WriteString("You have the following specialized skills. You can call the `load_skill` tool to read the full instructions of a skill if not already shown below, or follow the skill instructions directly:\n\n")
		for _, skill := range p.Skills {
			sb.WriteString(fmt.Sprintf("## Skill: %s\n", skill.Name))
			if skill.Description != "" {
				sb.WriteString(fmt.Sprintf("Description: %s\n\n", skill.Description))
			}
			sb.WriteString(strings.TrimSpace(skill.Content))
			sb.WriteString("\n\n")
		}
	}

	return strings.TrimSpace(sb.String())
}

// LoadFromDir loads a single Persona from an OS filesystem subfolder.
func LoadFromDir(dir string) (*Persona, error) {
	p, err := LoadFromBillyFS(osfs.New(dir), ".", filepath.Base(dir))
	if err != nil {
		return nil, err
	}
	p.DirPath = dir
	return p, nil
}

// LoadFromBillyFS loads a single Persona from a billy.Filesystem (either local OS or go-git in-memory).
func LoadFromBillyFS(fsys billy.Filesystem, dir string, fallbackID string) (*Persona, error) {
	if fallbackID == "" || fallbackID == "." || fallbackID == "/" {
		fallbackID = filepath.Base(dir)
	}
	hasher := sha256.New()

	// Locate config.yaml or persona.yaml
	var cfgPath string
	for _, candidate := range []string{"config.yaml", "config.yml", "persona.yaml", "persona.yml"} {
		p := fsys.Join(dir, candidate)
		if info, err := fsys.Stat(p); err == nil && !info.IsDir() {
			cfgPath = p
			break
		}
	}
	if cfgPath == "" {
		return nil, fmt.Errorf("no config.yaml or persona.yaml found in %s", dir)
	}

	rawCfg, err := readFileBilly(fsys, cfgPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", cfgPath, err)
	}
	hasher.Write(rawCfg)

	var p Persona
	if err := yaml.Unmarshal(rawCfg, &p); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", cfgPath, err)
	}

	if p.ID == "" {
		p.ID = fallbackID
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	p.DirPath = dir

	// Load base instructions from prompt.md or instructions.md if present
	for _, promptFile := range []string{"prompt.md", "instructions.md", "SYSTEM.md"} {
		promptPath := fsys.Join(dir, promptFile)
		if data, err := readFileBilly(fsys, promptPath); err == nil {
			hasher.Write(data)
			if p.Instructions != "" {
				p.Instructions = strings.TrimSpace(p.Instructions) + "\n\n" + strings.TrimSpace(string(data))
			} else {
				p.Instructions = strings.TrimSpace(string(data))
			}
			break
		}
	}

	// Load skills from skills/ subdirectory if present
	skillsDir := fsys.Join(dir, "skills")
	if info, err := fsys.Stat(skillsDir); err == nil && info.IsDir() {
		skills, err := loadSkillsFromBillyFS(fsys, skillsDir, hasher)
		if err != nil {
			return nil, fmt.Errorf("loading skills in %s: %w", skillsDir, err)
		}
		p.Skills = skills
	}

	p.ContentHash = hex.EncodeToString(hasher.Sum(nil))
	p.LoadedAt = time.Now().UTC()
	return &p, nil
}

func loadSkillsFromBillyFS(fsys billy.Filesystem, skillsDir string, hasher interface{ Write([]byte) (int, error) }) ([]Skill, error) {
	entries, err := fsys.ReadDir(skillsDir)
	if err != nil {
		return nil, err
	}

	var skills []Skill
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		fullPath := fsys.Join(skillsDir, entry.Name())
		if entry.IsDir() {
			// Check for SKILL.md inside subfolder
			skillMd := fsys.Join(fullPath, "SKILL.md")
			if data, err := readFileBilly(fsys, skillMd); err == nil {
				hasher.Write(data)
				skills = append(skills, parseSkillFile(entry.Name(), skillMd, string(data)))
			}
			continue
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			data, err := readFileBilly(fsys, fullPath)
			if err != nil {
				return nil, err
			}
			hasher.Write(data)
			defaultName := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			skills = append(skills, parseSkillFile(defaultName, fullPath, string(data)))
		}
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})
	return skills, nil
}

func readFileBilly(fsys billy.Filesystem, path string) ([]byte, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func parseSkillFile(defaultName, filePath, raw string) Skill {
	skill := Skill{
		Name:     defaultName,
		FilePath: filePath,
		Content:  strings.TrimSpace(raw),
	}

	// Check for optional YAML frontmatter: --- ... ---
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "---\n") || strings.HasPrefix(trimmed, "---\r\n") {
		rest := trimmed[4:]
		if idx := strings.Index(rest, "\n---"); idx != -1 {
			frontmatter := rest[:idx]
			body := strings.TrimSpace(rest[idx+4:])
			var meta struct {
				Name        string `yaml:"name"`
				Description string `yaml:"description"`
			}
			if err := yaml.Unmarshal([]byte(frontmatter), &meta); err == nil {
				if meta.Name != "" {
					skill.Name = meta.Name
				}
				if meta.Description != "" {
					skill.Description = meta.Description
				}
				skill.Content = body
			}
		}
	}

	// If no description in frontmatter, extract first paragraph or heading
	if skill.Description == "" {
		for _, line := range strings.Split(skill.Content, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if len(line) > 140 {
				line = line[:140] + "..."
			}
			skill.Description = line
			break
		}
	}

	return skill
}
