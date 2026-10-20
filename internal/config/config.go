// Package config loads harness.yaml: YAML parsing, ${VAR} expansion and the
// startup checks that stop a bad config before anything is served.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed example.yaml
var ExampleYAML string

type Config struct {
	Server      Server               `yaml:"server"`
	Auth        *Auth                `yaml:"auth"`
	Storage     Storage              `yaml:"storage"`
	Queue       Queue                `yaml:"queue"`
	Retention   Retention            `yaml:"retention"`
	Providers   map[string]Provider  `yaml:"providers"`
	MCP         map[string]MCPServer `yaml:"mcp"`
	Embeddings  *Embeddings          `yaml:"embeddings"`
	Attachments *Attachments         `yaml:"attachments"`
	Agent       Agent                `yaml:"agent"`
	Subagents   map[string]Subagent  `yaml:"subagents"`
	Summarizer  *Summarizer          `yaml:"summarizer"`
	Metrics     *Metrics             `yaml:"metrics"`
}

// Metrics is the operational listener every process (serve and queue:work)
// opens: /metrics, /healthz and /readyz, kept off the public API port.
type Metrics struct {
	Listen string `yaml:"listen"`
}

type Server struct {
	Listen string `yaml:"listen"`
	Secret string `yaml:"secret"`
}

type Auth struct {
	Webhook string   `yaml:"webhook"`
	Timeout Duration `yaml:"timeout"`
}

type Storage struct {
	URL string `yaml:"url"`
}

type Queue struct {
	Workers     int      `yaml:"workers"`
	Lease       Duration `yaml:"lease"`
	MaxAttempts int      `yaml:"max_attempts"`
}

type Retention struct {
	Trace Duration `yaml:"trace"`
}

type Provider struct {
	URL     string   `yaml:"url"`
	APIKey  string   `yaml:"api_key"`
	Timeout Duration `yaml:"timeout"`
}

type MCPServer struct {
	Type        string            `yaml:"type"` // local | remote
	Command     []string          `yaml:"command"`
	Environment map[string]string `yaml:"environment"`
	URL         string            `yaml:"url"`
	Headers     map[string]string `yaml:"headers"`
}

type Embeddings struct {
	Model      string `yaml:"model"`
	Dimensions int    `yaml:"dimensions"`
}

type Attachments struct {
	AllowedHosts []string `yaml:"allowed_hosts"`
	Types        []string `yaml:"types"`
	MaxBytes     ByteSize `yaml:"max_bytes"`
}

type Settings struct {
	Temperature   *float64 `yaml:"temperature"`
	TopP          *float64 `yaml:"top_p"`
	MaxTokens     *int     `yaml:"max_tokens"`
	ContextWindow int      `yaml:"context_window"`
	Vision        bool     `yaml:"vision"`
}

type Var struct {
	Required  bool    `yaml:"required"`
	Default   *string `yaml:"default"`
	MaxLength int     `yaml:"max_length"`
}

type Limits struct {
	Steps       int      `yaml:"steps"`
	TotalTokens int64    `yaml:"total_tokens"`
	Timeout     Duration `yaml:"timeout"`
}

type Approval struct {
	Tools   []string `yaml:"tools"`
	Timeout Duration `yaml:"timeout"`
}

type Agent struct {
	Model    string         `yaml:"model"`
	Settings Settings       `yaml:"settings"`
	System   string         `yaml:"system"`
	Vars     map[string]Var `yaml:"vars"`
	Limits   Limits         `yaml:"limits"`
	Approval *Approval      `yaml:"approval"`
	MCP      []string       `yaml:"mcp"` // nil = all servers, via tool search
}

type Subagent struct {
	Description string   `yaml:"description"`
	Model       string   `yaml:"model"`
	Settings    Settings `yaml:"settings"`
	System      string   `yaml:"system"`
	MCP         []string `yaml:"mcp"`
	Limits      Limits   `yaml:"limits"`
}

type Summarizer struct {
	Model      string   `yaml:"model"`
	Settings   Settings `yaml:"settings"`
	System     string   `yaml:"system"`
	KeepRecent int      `yaml:"keep_recent"`
}

// Load reads, expands and validates the config file.
func Load(file string) (*Config, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return Parse(raw, os.LookupEnv)
}

// Parse is Load without the file read; lookup resolves ${VAR}.
func Parse(raw []byte, lookup func(string) (string, bool)) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if root.Kind == 0 {
		return nil, errors.New("config is empty")
	}
	if err := checkKeys(&root, reflect.TypeFor[Config](), "config"); err != nil {
		return nil, err
	}
	if err := expandNode(&root, lookup); err != nil {
		return nil, err
	}
	var c Config
	if err := root.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// expandNode expands ${VAR} in string values only, never in keys or structure.
func expandNode(n *yaml.Node, lookup func(string) (string, bool)) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag != "!!str" || !strings.Contains(n.Value, "$") {
			return nil
		}
		v, err := Expand(n.Value, lookup)
		if err != nil {
			return err
		}
		n.Value = v
		if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0 {
			n.Style, n.Tag = 0, "" // let numbers and bools re-resolve
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 { // values only
			if err := expandNode(n.Content[i], lookup); err != nil {
				return err
			}
		}
	default:
		for _, c := range n.Content {
			if err := expandNode(c, lookup); err != nil {
				return err
			}
		}
	}
	return nil
}

// Expand replaces ${VAR} and ${VAR:-default}; "$$" is a literal "$".
// A missing variable without a default is an error naming the variable.
func Expand(s string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated ${ in %q", s)
			}
			expr := s[i+2 : i+end]
			name, def, hasDef := strings.Cut(expr, ":-")
			if !envName.MatchString(name) {
				return "", fmt.Errorf("invalid variable name %q", name)
			}
			v, ok := lookup(name)
			switch {
			case ok && (v != "" || !hasDef):
				b.WriteString(v)
			case hasDef:
				b.WriteString(def)
			default:
				return "", fmt.Errorf("environment variable %s is not set", name)
			}
			i += end
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), nil
}

var (
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	nameRe     = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	varNameRe  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	placeholRe = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
)

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Auth != nil && c.Auth.Timeout == 0 {
		c.Auth.Timeout = Duration(2 * time.Second)
	}
	if c.Queue.Workers == 0 {
		c.Queue.Workers = 4
	}
	if c.Queue.Lease == 0 {
		c.Queue.Lease = Duration(30 * time.Second)
	}
	if c.Queue.MaxAttempts == 0 {
		c.Queue.MaxAttempts = 3
	}
	if c.Retention.Trace == 0 {
		c.Retention.Trace = Duration(30 * 24 * time.Hour)
	}
	for k, p := range c.Providers {
		if p.Timeout == 0 {
			p.Timeout = Duration(60 * time.Second)
		}
		c.Providers[k] = p
	}
	if c.Agent.Approval != nil && c.Agent.Approval.Timeout == 0 {
		c.Agent.Approval.Timeout = Duration(10 * time.Minute)
	}
	if c.Summarizer != nil && c.Summarizer.KeepRecent == 0 {
		c.Summarizer.KeepRecent = 3
	}
	if c.Attachments != nil && c.Attachments.MaxBytes == 0 {
		c.Attachments.MaxBytes = 5 << 20
	}
}

// Validate runs the startup rules; every error names what to fix.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Server.Secret == "" {
		bad("server.secret is required")
	}
	if c.Storage.URL == "" {
		bad("storage.url is required")
	}
	if c.Auth != nil && c.Auth.Webhook == "" {
		bad("auth.webhook is required when auth is set")
	}
	if c.Metrics != nil && c.Metrics.Listen == "" {
		bad("metrics.listen is required when metrics is set")
	}
	if c.Queue.Workers < 1 || c.Queue.MaxAttempts < 1 {
		bad("queue.workers and queue.max_attempts must be at least 1")
	}

	for name, p := range c.Providers {
		if !nameRe.MatchString(name) {
			bad("providers.%s: invalid name", name)
		}
		if p.URL == "" {
			bad("providers.%s.url is required", name)
		}
	}
	checkModel := func(where, model string) {
		prov, m, ok := strings.Cut(model, "/")
		if !ok || m == "" {
			bad("%s.model %q must look like <provider>/<model>", where, model)
			return
		}
		if _, exists := c.Providers[prov]; !exists {
			bad("%s.model: unknown provider %q", where, prov)
		}
	}

	for name, s := range c.MCP {
		if !nameRe.MatchString(name) || strings.Contains(name, "__") {
			bad("mcp.%s: names use [a-zA-Z0-9_-] and no double underscore", name)
		}
		switch s.Type {
		case "local":
			if len(s.Command) == 0 {
				bad("mcp.%s.command is required for type local", name)
			}
		case "remote":
			if s.URL == "" {
				bad("mcp.%s.url is required for type remote", name)
			}
		default:
			bad("mcp.%s.type must be local or remote", name)
		}
	}
	checkMCP := func(where string, refs []string) {
		for _, r := range refs {
			if _, ok := c.MCP[r]; !ok {
				bad("%s.mcp: unknown server %q", where, r)
			}
		}
	}

	if c.Embeddings != nil {
		checkModel("embeddings", c.Embeddings.Model)
		if c.Embeddings.Dimensions < 1 || c.Embeddings.Dimensions > 4000 {
			bad("embeddings.dimensions must be between 1 and 4000")
		}
	}
	if a := c.Attachments; a != nil {
		if len(a.AllowedHosts) == 0 || len(a.Types) == 0 {
			bad("attachments needs allowed_hosts and types")
		}
	}

	// agent
	checkModel("agent", c.Agent.Model)
	if c.Agent.Settings.ContextWindow < 1 {
		bad("agent.settings.context_window is required")
	}
	checkMCP("agent", c.Agent.MCP)
	for n, v := range c.Agent.Vars {
		if !varNameRe.MatchString(n) {
			bad("agent.vars.%s: invalid name", n)
		}
		if v.Required && v.Default != nil {
			bad("agent.vars.%s: required and default are mutually exclusive", n)
		}
	}
	checkPlaceholders := func(where, text string, vars map[string]Var) {
		for _, m := range placeholRe.FindAllStringSubmatch(text, -1) {
			if _, ok := vars[m[1]]; !ok {
				bad("%s.system uses {{%s}} which is not declared under agent.vars", where, m[1])
			}
		}
	}
	checkPlaceholders("agent", c.Agent.System, c.Agent.Vars)
	if c.Agent.Approval != nil {
		for _, p := range c.Agent.Approval.Tools {
			if _, err := path.Match(p, ""); err != nil {
				bad("agent.approval.tools: bad pattern %q", p)
			}
		}
	}

	for name, s := range c.Subagents {
		where := "subagents." + name
		if name == "main" || name == "summarizer" {
			bad("%s: name is reserved", where)
		}
		if !nameRe.MatchString(name) || strings.Contains(name, "__") {
			bad("%s: names use [a-zA-Z0-9_-] and no double underscore", where)
		}
		if s.Description == "" {
			bad("%s.description is required", where)
		}
		checkModel(where, s.Model)
		checkMCP(where, s.MCP)
		if strings.Contains(s.System, "{{") {
			bad("%s.system: sub-agents receive no vars, so {{...}} is not allowed", where)
		}
	}

	if s := c.Summarizer; s != nil {
		checkModel("summarizer", s.Model)
		if s.Settings.ContextWindow < 1 {
			bad("summarizer.settings.context_window is required")
		}
		if s.KeepRecent < 0 {
			bad("summarizer.keep_recent must not be negative")
		}
	}

	return errors.Join(errs...)
}

// SplitModel splits "<provider>/<model>" (the model part may contain slashes).
func SplitModel(model string) (provider, name string) {
	provider, name, _ = strings.Cut(model, "/")
	return
}

// SortedKeys returns map keys in a stable order.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
