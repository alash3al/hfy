// Package config loads hfy.yaml: YAML parsing, ${VAR} expansion and the
// startup checks that stop a bad config before anything is served.
package config

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/alash3al/hfy/internal/types"
)

//go:embed example.yaml
var ExampleYAML string

type Config struct {
	Server      Server               `yaml:"server"`
	Database    Database             `yaml:"database"`
	Queue       Queue                `yaml:"queue"`
	Retention   Retention            `yaml:"retention"`
	Providers   map[string]Provider  `yaml:"providers"`
	MCP         map[string]MCPServer `yaml:"mcp"`
	Embeddings  *Embeddings          `yaml:"embeddings"`
	Attachments *Attachments         `yaml:"attachments"`
	Filesystem  *Filesystem          `yaml:"filesystem"`
	Agent       Agent                `yaml:"agent"`  // the default agent, named "main"
	Agents      map[string]Agent     `yaml:"agents"` // more agents; a grant picks one by name
	Subagents   map[string]Subagent  `yaml:"subagents"`
	Summarizer  *Summarizer          `yaml:"summarizer"`
	Metrics     *Metrics             `yaml:"metrics"`
	Embed       Embed                `yaml:"embed"`
}

type Embed struct {
	Lang    string `yaml:"lang"`
	Dir     string `yaml:"dir"`
	Theme   string `yaml:"theme"`
	Accent  string `yaml:"accent"`
	Density string `yaml:"density"`
	Layout  string `yaml:"layout"`
	// ToolCalls is how much of the agent's tool use viewers see: hidden,
	// summary (a friendly row, no JSON) or details (arguments and results).
	// Approval requests are always shown: the viewer must decide them.
	ToolCalls string `yaml:"tool_calls"`
}

// Metrics is the operational listener every process (serve and queue:work)
// opens: /metrics, /healthz and /readyz, kept off the public API port.
type Metrics struct {
	Listen string `yaml:"listen"`
}

type Server struct {
	Listen string `yaml:"listen"`
	Secret string `yaml:"secret"`
	// CORSOrigins are the web origins whose pages may call the client routes
	// (["*"] for any). The backend routes never answer a browser.
	CORSOrigins []string `yaml:"cors_origins"`
}

type Database struct {
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
	Type    string       `yaml:"type"` // openai | mock; empty means openai
	URL     string       `yaml:"url"`
	APIKey  string       `yaml:"api_key"`
	Timeout Duration     `yaml:"timeout"`
	Mock    MockProvider `yaml:"mock"`
}

type MockProvider struct {
	Reply           string        `yaml:"reply"`
	Reasoning       string        `yaml:"reasoning"`
	Delay           Duration      `yaml:"delay"`
	ErrorStatus     int           `yaml:"error_status"`
	EmbedDimensions int           `yaml:"embed_dimensions"`
	ToolCall        *MockToolCall `yaml:"tool_call"`
}

type MockToolCall struct {
	Name      string `yaml:"name"`
	Arguments string `yaml:"arguments"` // JSON object text
}

type MCPServer struct {
	Type        string            `yaml:"type"` // local | remote
	Command     []string          `yaml:"command"`
	Environment map[string]string `yaml:"environment"` // a local server's only variables besides PATH, HOME and locale
	URL         string            `yaml:"url"`
	Headers     map[string]string `yaml:"headers"`
	Timeout     Duration          `yaml:"timeout"` // longest a tool call may take
}

type Embeddings struct {
	Model      string `yaml:"model"`
	Dimensions int    `yaml:"dimensions"`
}

type Attachments struct {
	AllowedHosts []string `yaml:"allowed_hosts"`
	Types        []string `yaml:"types"`
	MaxBytes     ByteSize `yaml:"max_bytes"`
	MaxCount     int      `yaml:"max_count"` // per message
}

type Filesystem struct {
	DSN         string   `yaml:"dsn"`
	PublicURL   string   `yaml:"public_url"`
	MaxBytes    ByteSize `yaml:"max_bytes"`
	Types       []string `yaml:"types"`
	UploadTTL   Duration `yaml:"upload_ttl"`
	DownloadTTL Duration `yaml:"download_ttl"`
}

type Settings struct {
	Temperature   *float64 `yaml:"temperature"`
	TopP          *float64 `yaml:"top_p"`
	ContextWindow int      `yaml:"context_window"`
	Vision        bool     `yaml:"vision"`
}

// Var is a {{name}} in a system prompt. Values come only from the grant the
// app's backend created (a client never sets them), else from defaults.
type Var struct {
	Required bool    `yaml:"required"`
	Default  *string `yaml:"default"`
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

// Requires reports whether a call of tool (<server>.<tool>) waits for
// approval; a nil policy requires none.
func (a *Approval) Requires(tool string) bool {
	if a == nil {
		return false
	}
	for _, p := range a.Tools {
		if ok, _ := path.Match(p, tool); ok {
			return true
		}
	}
	return false
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
	c.SetDefaults()
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
	accentRe   = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	langRe     = regexp.MustCompile(`^[a-zA-Z]{2,3}(-[a-zA-Z0-9]{2,8})*$`)
)

// Render substitutes {{var}} placeholders; unknown names are left as written.
func Render(tmpl string, vars map[string]string) string {
	return placeholRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		name := placeholRe.FindStringSubmatch(m)[1]
		if v, ok := vars[name]; ok {
			return v
		}
		return m
	})
}

// SetDefaults fills in what the config leaves unset. Parse calls it; a Config
// built by hand must too, as nothing else knows the defaults.
func (c *Config) SetDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
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
		if p.Type == "" {
			p.Type = "openai"
		}
		if p.Timeout == 0 {
			p.Timeout = Duration(60 * time.Second)
		}
		c.Providers[k] = p
	}
	if c.Embeddings != nil {
		prov, _ := SplitModel(c.Embeddings.Model)
		if p, ok := c.Providers[prov]; ok && p.Type == "mock" && p.Mock.EmbedDimensions == 0 {
			p.Mock.EmbedDimensions = c.Embeddings.Dimensions
			c.Providers[prov] = p
		}
	}
	for k, s := range c.MCP {
		if s.Timeout == 0 {
			s.Timeout = Duration(2 * time.Minute)
		}
		c.MCP[k] = s
	}
	if c.Agent.Approval != nil && c.Agent.Approval.Timeout == 0 {
		c.Agent.Approval.Timeout = Duration(10 * time.Minute)
	}
	for _, a := range c.Agents {
		if a.Approval != nil && a.Approval.Timeout == 0 {
			a.Approval.Timeout = Duration(10 * time.Minute) // a pointer: this sets the map's copy too
		}
	}
	if c.Summarizer != nil && c.Summarizer.KeepRecent == 0 {
		c.Summarizer.KeepRecent = 3
	}
	if c.Attachments != nil && c.Attachments.MaxBytes == 0 {
		c.Attachments.MaxBytes = 5 << 20
	}
	if c.Attachments != nil && c.Attachments.MaxCount == 0 {
		c.Attachments.MaxCount = 10
	}
	if f := c.Filesystem; f != nil {
		if f.MaxBytes == 0 {
			if c.Attachments != nil && c.Attachments.MaxBytes != 0 {
				f.MaxBytes = c.Attachments.MaxBytes
			} else {
				f.MaxBytes = 10 << 20
			}
		}
		if len(f.Types) == 0 && c.Attachments != nil {
			f.Types = slices.Clone(c.Attachments.Types)
		}
		if f.UploadTTL == 0 {
			f.UploadTTL = Duration(5 * time.Minute)
		}
		if f.DownloadTTL == 0 {
			f.DownloadTTL = Duration(15 * time.Minute)
		}
	}
	if c.Embed.Dir == "" {
		c.Embed.Dir = "auto"
	}
	if c.Embed.Theme == "" {
		c.Embed.Theme = "auto"
	}
	if c.Embed.Accent == "" {
		c.Embed.Accent = "#6366f1"
	}
	if c.Embed.Density == "" {
		c.Embed.Density = "comfortable"
	}
	if c.Embed.Layout == "" {
		c.Embed.Layout = "bubbles"
	}
	if c.Embed.ToolCalls == "" {
		c.Embed.ToolCalls = "summary"
	}
}

// Validate runs the startup rules; every error names what to fix.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Server.Secret == "" {
		bad("server.secret is required")
	}
	for _, o := range c.Server.CORSOrigins {
		if !validOrigin(o) || o == "*" && len(c.Server.CORSOrigins) > 1 {
			bad("server.cors_origins: %q must be scheme://host[:port] with no path or trailing slash, or \"*\" alone", o)
		}
	}
	if c.Database.URL == "" {
		bad("database.url is required")
	}
	if c.Metrics != nil && c.Metrics.Listen == "" {
		bad("metrics.listen is required when metrics is set")
	}
	if c.Queue.Workers < 1 || c.Queue.MaxAttempts < 1 {
		bad("queue.workers and queue.max_attempts must be at least 1")
	}
	if !slices.Contains([]string{"auto", "ltr", "rtl"}, c.Embed.Dir) {
		bad("embed.dir must be one of auto, ltr, rtl")
	}
	if !slices.Contains([]string{"light", "dark", "auto"}, c.Embed.Theme) {
		bad("embed.theme must be one of light, dark, auto")
	}
	if !accentRe.MatchString(c.Embed.Accent) {
		bad("embed.accent must be a hex color like #6366f1")
	}
	if !slices.Contains([]string{"comfortable", "compact"}, c.Embed.Density) {
		bad("embed.density must be one of comfortable, compact")
	}
	if !slices.Contains([]string{"bubbles", "transcript"}, c.Embed.Layout) {
		bad("embed.layout must be one of bubbles, transcript")
	}
	if !slices.Contains([]string{"hidden", "summary", "details"}, c.Embed.ToolCalls) {
		bad("embed.tool_calls must be one of hidden, summary, details")
	}
	if c.Embed.Lang != "" && !langRe.MatchString(c.Embed.Lang) {
		bad("embed.lang must be a language tag like en or en-US")
	}

	for name, p := range c.Providers {
		if !nameRe.MatchString(name) {
			bad("providers.%s: invalid name", name)
		}
		switch p.Type {
		case "openai":
			if p.URL == "" {
				bad("providers.%s.url is required", name)
			}
		case "mock":
			if p.Mock.ErrorStatus != 0 && (p.Mock.ErrorStatus < 400 || p.Mock.ErrorStatus > 599) {
				bad("providers.%s.mock.error_status must be 0 or between 400 and 599", name)
			}
			if p.Mock.EmbedDimensions < 0 || p.Mock.EmbedDimensions > 4000 {
				bad("providers.%s.mock.embed_dimensions must be 0 or between 1 and 4000", name)
			}
			if p.Mock.Delay < 0 {
				bad("providers.%s.mock.delay must not be negative", name)
			}
			if tc := p.Mock.ToolCall; tc != nil {
				if tc.Name == "" {
					bad("providers.%s.mock.tool_call.name is required", name)
				}
				if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
					bad("providers.%s.mock.tool_call.arguments must be valid JSON", name)
				}
			}
		default:
			bad("providers.%s.type must be openai or mock", name)
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

	if _, taken := c.MCP[BuiltinServer]; taken {
		bad("mcp.%s: the name is reserved for hfy's built-in tools", BuiltinServer)
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
		if prov, _ := SplitModel(c.Embeddings.Model); c.Providers[prov].Type == "mock" &&
			c.Providers[prov].Mock.EmbedDimensions != 0 && c.Providers[prov].Mock.EmbedDimensions != c.Embeddings.Dimensions {
			bad("embeddings.dimensions does not match providers.%s.mock.embed_dimensions", prov)
		}
	}
	if a := c.Attachments; a != nil {
		if len(a.AllowedHosts) == 0 || len(a.Types) == 0 {
			bad("attachments needs allowed_hosts and types")
		}
	}
	if f := c.Filesystem; f != nil {
		if f.DSN == "" {
			bad("filesystem.dsn is required")
		} else if scheme, _, _ := strings.Cut(f.DSN, "://"); !slices.Contains([]string{"s3", "file"}, scheme) {
			bad("filesystem.dsn must use s3:// or file:// (mem:// is for tests, injected directly)")
		} else if scheme == "s3" && !dsnHasRegion(f.DSN) {
			bad("filesystem.dsn: an s3:// DSN must carry region=<region> in its query, e.g. s3://bucket?region=eu-west-1")
		}
		if len(f.Types) == 0 {
			bad("filesystem.types is required")
		}
		if f.MaxBytes <= 0 {
			bad("filesystem.max_bytes must be greater than 0")
		}
		if f.UploadTTL <= 0 || f.UploadTTL > Duration(time.Hour) {
			bad("filesystem.upload_ttl must be greater than 0 and at most 1h")
		}
		if f.DownloadTTL <= 0 || f.DownloadTTL > Duration(24*time.Hour) {
			bad("filesystem.download_ttl must be greater than 0 and at most 24h")
		}
		if f.PublicURL != "" {
			if u, err := url.Parse(f.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				bad("filesystem.public_url must be an http(s) URL with a host")
			}
		}
	}

	// agents: "agent" (named main) and every entry under "agents"
	agents := c.AllAgents()
	for _, name := range slices.Sorted(maps.Keys(agents)) {
		a := agents[name]
		where := "agents." + name
		if name == types.AgentMain {
			where = "agent"
		} else if name == types.AgentSummarizer || !nameRe.MatchString(name) || strings.Contains(name, "__") {
			bad("%s: name is reserved or invalid (use [a-zA-Z0-9_-], no double underscore)", where)
		}
		checkModel(where, a.Model)
		if a.Settings.ContextWindow < 1 {
			bad("%s.settings.context_window is required", where)
		}
		checkMCP(where, a.MCP)
		for n, v := range a.Vars {
			if !varNameRe.MatchString(n) {
				bad("%s.vars.%s: invalid name", where, n)
			}
			if v.Required && v.Default != nil {
				bad("%s.vars.%s: required and default are mutually exclusive", where, n)
			}
		}
		for _, m := range placeholRe.FindAllStringSubmatch(a.System, -1) {
			if _, ok := a.Vars[m[1]]; !ok {
				bad("%s.system uses {{%s}} which is not declared under %s.vars", where, m[1], where)
			}
		}
		if a.Approval != nil {
			for _, p := range a.Approval.Tools {
				if _, err := path.Match(p, ""); err != nil {
					bad("%s.approval.tools: bad pattern %q", where, p)
				}
			}
		}
	}

	for name, s := range c.Subagents {
		where := "subagents." + name
		if _, taken := agents[name]; taken || name == types.AgentSummarizer {
			bad("%s: name is reserved or taken by an agent (agents and sub-agents share one namespace)", where)
		}
		if slices.Contains(BuiltinTools, name) {
			bad("%s: the name is taken by the built-in tool hfy.%s", where, name)
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

// validOrigin reports whether o is written as a browser sends its Origin
// header, so it can ever match: "*", or lowercase http(s)://host[:port] with
// no path, no trailing slash and no default port.
func validOrigin(o string) bool {
	if o == "*" {
		return true
	}
	u, err := url.Parse(o)
	defaultPort := u != nil && (u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80")
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && !defaultPort &&
		u.String() == u.Scheme+"://"+u.Host && strings.ToLower(o) == o
}

// dsnHasRegion reports whether an s3:// DSN names a region in its query; the
// S3 driver cannot sign requests without one.
func dsnHasRegion(dsn string) bool {
	u, err := url.Parse(dsn)
	return err == nil && u.Query().Get("region") != ""
}

// BuiltinServer is the server part of hfy's own tools' names (hfy.now,
// hfy.tool_search, sub-agents as hfy.<name>); no configured server may take it.
const BuiltinServer = "hfy"

// BuiltinTools are the names hfy's own tools take on that server, so no
// sub-agent may (a test in package builtins keeps the list complete).
var BuiltinTools = []string{"tool_search", "conversation_search", "now", "math_calc"}

// AllAgents returns every agent by name; the default "agent" is "main".
func (c *Config) AllAgents() map[string]*Agent {
	all := map[string]*Agent{types.AgentMain: &c.Agent}
	for name, a := range c.Agents {
		all[name] = &a
	}
	return all
}

// AgentNamed returns the agent a grant picked ("" = main).
func (c *Config) AgentNamed(name string) (*Agent, bool) {
	if name == "" {
		name = types.AgentMain
	}
	a, ok := c.AllAgents()[name]
	return a, ok
}

// SplitModel splits "<provider>/<model>" (the model part may contain slashes).
func SplitModel(model string) (provider, name string) {
	provider, name, _ = strings.Cut(model, "/")
	return
}
