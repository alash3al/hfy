// Package config loads hfy.yaml: YAML parsing, ${VAR} expansion and the
// startup checks that stop a bad config before anything is served.
package config

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/alash3al/hfy/internal/types"
)

//go:embed example.yaml
var ExampleYAML string

type Config struct {
	Server     Server               `yaml:"server"`
	Database   Database             `yaml:"database"`
	Queue      Queue                `yaml:"queue"`
	Retention  Retention            `yaml:"retention"`
	Providers  map[string]Provider  `yaml:"providers"`
	MCP        map[string]MCPServer `yaml:"mcp"`
	Files      *Files               `yaml:"files"`
	Agents     map[string]*Agent    `yaml:"agents"` // required: "main" answers a grant that names no agent
	Subagents  map[string]Subagent  `yaml:"subagents"`
	Summarizer *Summarizer          `yaml:"summarizer"`
	Metrics    *Metrics             `yaml:"metrics"`
	Embed      Embed                `yaml:"embed"`
}

type Embed struct {
	Lang   string `yaml:"lang"`
	Dir    string `yaml:"dir"`
	Theme  string `yaml:"theme"`
	Accent string `yaml:"accent"`
	// AccentSet is true when the accent was written, even as the default indigo: the
	// whatsapp layout uses its own green unless an accent is set.
	AccentSet bool `yaml:"-"`
	// accentDefaulted remembers that SetDefaults filled the accent in, so AccentSet
	// stays false when SetDefaults runs again on the same config.
	accentDefaulted bool
	Density         string `yaml:"density"`
	Layout          string `yaml:"layout"`
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
	Type    string   `yaml:"type"` // openai | mock; empty means openai
	URL     string   `yaml:"url"`
	APIKey  string   `yaml:"api_key"`
	Timeout Duration `yaml:"timeout"`
	// MaxTokensParam names the output-cap field of openai providers: max_tokens (default) or max_completion_tokens.
	MaxTokensParam string       `yaml:"max_tokens_param"`
	Mock           MockProvider `yaml:"mock"`
}

type MockProvider struct {
	Reply       string        `yaml:"reply"`
	Reasoning   string        `yaml:"reasoning"`
	Delay       Duration      `yaml:"delay"`
	ErrorStatus int           `yaml:"error_status"`
	ToolCall    *MockToolCall `yaml:"tool_call"`
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
	// Approval is server-owned policy: tool-name patterns WITHOUT the server
	// prefix (path.Match syntax, e.g. "refund", "delete_*"); calls to matching
	// tools of this server wait for approval, for EVERY agent.
	Approval []string `yaml:"approval"`
	// ApproveDestructive waits for approval on tools whose MCP annotations
	// (readOnlyHint not true, destructiveHint explicitly true) say they may
	// destroy things.
	ApproveDestructive bool `yaml:"approve_destructive"`
	// UI opts this server's tools into returning cards (mcp _meta "hfy/ui")
	// and tool-triggered questions. Nil (absent) = both dropped.
	UI *UI `yaml:"ui"`
}

// UI enables the ui block one MCP server's tools may return. ImageHosts
// filters the image URLs a card may show ("*.suffix" patterns allowed); an
// empty list drops images.
type UI struct {
	ImageHosts []string `yaml:"image_hosts"`
}

// Files is everything that lets files reach a conversation. Two sources share
// Types and MaxCount: URL attachments (AllowedHosts set; hfy never fetches
// them, so MaxBytes does not apply) and uploads (DSN set; hfy stores them).
type Files struct {
	Types        []string `yaml:"types"`
	MaxCount     int      `yaml:"max_count"` // per message, both sources
	MaxBytes     ByteSize `yaml:"max_bytes"` // per uploaded file
	AllowedHosts []string `yaml:"allowed_hosts"`
	DSN          string   `yaml:"dsn"`
	PublicURL    string   `yaml:"public_url"`
	UploadTTL    Duration `yaml:"upload_ttl"`
	DownloadTTL  Duration `yaml:"download_ttl"`
}

// URLs reports whether URL attachments are on.
func (f *Files) URLs() bool { return f != nil && len(f.AllowedHosts) > 0 }

// Uploads reports whether file uploads are on.
func (f *Files) Uploads() bool { return f != nil && f.DSN != "" }

// Settings are one agent's model settings; sub-agents and the summarizer have
// the same. The request controls are optional: unset, nothing is sent.
type Settings struct {
	Temperature   *float64 `yaml:"temperature"`
	TopP          *float64 `yaml:"top_p"`
	ContextWindow int      `yaml:"context_window"`
	Vision        bool     `yaml:"vision"`
	// ReasoningEffort is sent as reasoning_effort: none, minimal, low, medium, high, xhigh or max.
	ReasoningEffort string `yaml:"reasoning_effort"`
	// MaxOutputTokens caps each call's output, reasoning included.
	MaxOutputTokens int `yaml:"max_output_tokens"`
	// CallTimeout bounds a whole model call, retries included (0 = none).
	CallTimeout Duration `yaml:"call_timeout"`
	// Extra is merged into the request body as the provider's own fields.
	// It is stored in outbound_requests, so it never holds secrets.
	Extra map[string]any `yaml:"extra"`
}

// reasoningEfforts are the reasoning_effort values OpenAI's Chat Completions
// reference lists; a model may accept only some of them.
var reasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

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
	// AskUser opts the agent into hfy.ask_user: it may pause a turn asking
	// the viewer structured questions. Off by default: an API-only app
	// without a UI would stall on questions it cannot render.
	AskUser bool `yaml:"ask_user"`
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
	if err := checkFilesNode(root.Content[0]); err != nil {
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

// checkFilesNode rejects, after ${VAR} expansion, what decoding would quietly
// turn into "off" or "the default": an empty or null dsn (it is the uploads
// switch), and an explicit zero (or a float, which yaml truncates) in the
// limits. A key that is absent still means the default.
func checkFilesNode(top *yaml.Node) error {
	for _, k := range []string{"dsn", "max_count", "max_bytes", "upload_ttl", "download_ttl"} {
		v := lookupKey(top, "files", k)
		if v == nil || v.Kind != yaml.ScalarNode {
			continue
		}
		if k == "dsn" {
			if v.Value == "" || v.ShortTag() == "!!null" {
				return fmt.Errorf("line %d: files.dsn is empty; remove the key to turn uploads off", v.Line)
			}
			continue
		}
		if v.Value == "" || v.ShortTag() == "!!null" {
			continue // bare or null: unset
		}
		switch k {
		case "max_count":
			if n, err := strconv.Atoi(v.Value); err != nil || n <= 0 {
				return fmt.Errorf("line %d: files.max_count must be a whole number greater than 0", v.Line)
			}
		case "max_bytes":
			if n, err := ParseBytes(v.Value); err == nil && n == 0 {
				return fmt.Errorf("line %d: files.max_bytes must be greater than 0", v.Line)
			}
		default:
			if d, err := ParseDuration(v.Value); err == nil && d == 0 {
				return fmt.Errorf("line %d: files.%s must be greater than 0", v.Line, k)
			}
		}
	}
	return nil
}

// lookupKey follows mapping keys from n (through YAML aliases), or returns nil.
func lookupKey(n *yaml.Node, keys ...string) *yaml.Node {
	deref := func(n *yaml.Node) *yaml.Node {
		if n != nil && n.Kind == yaml.AliasNode {
			return n.Alias
		}
		return n
	}
	n = deref(n)
	for _, k := range keys {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		next := (*yaml.Node)(nil)
		for kv := range slices.Chunk(n.Content, 2) {
			if kv[0].Value == k {
				next = kv[1]
			}
		}
		n = deref(next)
	}
	return n
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

// ValidName reports whether s is a name config accepts for an agent, a server
// and the like: ASCII letters, digits, "_" and "-". A host's source name takes
// the same rule, so it can sit in a prompt attribute without any quoting.
func ValidName(s string) bool { return nameRe.MatchString(s) }

var (
	envName       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	nameRe        = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	varNameRe     = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	placeholRe    = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
	accentRe      = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	langRe        = regexp.MustCompile(`^[a-zA-Z]{2,3}(-[a-zA-Z0-9]{2,8})*$`)
	hostPatternRe = regexp.MustCompile(`^(\*\.)?([a-z0-9-]+\.)+[a-z]{2,}$`)
	mimeRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9.+_-]*/[a-z0-9][a-z0-9.+_-]*$`)
	hostLabelsRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)

// validAllowedHost accepts what url.Hostname() can return for an http(s) URL:
// a lowercase DNS name (localhost included), an IP, or "*.name" with at least
// two labels so "*.com" cannot happen by accident. A name whose last label
// starts with a digit is refused: no TLD does, and "*.0.0.1" or "1.2.3" would
// be IPv4 in disguise ("127.0.0.1" matching a pattern).
func validAllowedHost(h string) bool {
	if net.ParseIP(h) != nil {
		return true
	}
	name, wild := strings.CutPrefix(h, "*.")
	if wild && !strings.Contains(name, ".") {
		return false
	}
	last := name[strings.LastIndexByte(name, '.')+1:]
	return hostLabelsRe.MatchString(name) && last[0] > '9'
}

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
		if p.MaxTokensParam == "" {
			p.MaxTokensParam = "max_tokens"
		}
		c.Providers[k] = p
	}
	for k, s := range c.MCP {
		if s.Timeout == 0 {
			s.Timeout = Duration(2 * time.Minute)
		}
		c.MCP[k] = s
	}
	for name, a := range c.Agents {
		if a == nil { // "main:" with no value; Validate reports what it lacks
			a = &Agent{}
			c.Agents[name] = a
		}
		if a.Approval != nil && a.Approval.Timeout == 0 {
			a.Approval.Timeout = Duration(10 * time.Minute)
		}
	}
	if c.Summarizer != nil && c.Summarizer.KeepRecent == 0 {
		c.Summarizer.KeepRecent = 3
	}
	if f := c.Files; f != nil {
		if f.MaxCount == 0 {
			f.MaxCount = 10
		}
		if f.MaxBytes == 0 {
			f.MaxBytes = 10 << 20
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
		c.Embed.accentDefaulted = true
	}
	c.Embed.AccentSet = !c.Embed.accentDefaulted
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
	if !slices.Contains([]string{"comfortable", "compact", "spacious"}, c.Embed.Density) {
		bad("embed.density must be one of comfortable, compact, spacious")
	}
	if !slices.Contains([]string{"bubbles", "transcript", "whatsapp"}, c.Embed.Layout) {
		bad("embed.layout must be one of bubbles, transcript, whatsapp")
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
			if !slices.Contains([]string{"max_tokens", "max_completion_tokens"}, p.MaxTokensParam) {
				bad("providers.%s.max_tokens_param must be max_tokens or max_completion_tokens", name)
			}
		case "mock":
			if p.Mock.ErrorStatus != 0 && (p.Mock.ErrorStatus < 400 || p.Mock.ErrorStatus > 599) {
				bad("providers.%s.mock.error_status must be 0 or between 400 and 599", name)
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

	// checkControls validates the request controls of the settings at where
	// (agent, sub-agent or summarizer).
	checkControls := func(where string, s Settings) {
		if s.ReasoningEffort != "" && !slices.Contains(reasoningEfforts, s.ReasoningEffort) {
			bad("%s.settings.reasoning_effort must be one of %s", where, strings.Join(reasoningEfforts, ", "))
		}
		if s.MaxOutputTokens < 0 {
			bad("%s.settings.max_output_tokens must be greater than 0 (omit it for no cap)", where)
		}
		if s.CallTimeout < 0 {
			bad("%s.settings.call_timeout must not be negative", where)
		}
		if _, err := json.Marshal(s.Extra); err != nil {
			bad("%s.settings.extra: values must be JSON-serialisable: %v", where, err)
		}
		// the keys the settings above already send: extra may not set them again
		owner := map[string]string{}
		if s.ReasoningEffort != "" {
			owner["reasoning_effort"] = "reasoning_effort"
		}
		if s.Temperature != nil {
			owner["temperature"] = "temperature"
		}
		if s.TopP != nil {
			owner["top_p"] = "top_p"
		}
		if s.MaxOutputTokens > 0 {
			owner["max_tokens"], owner["max_completion_tokens"] = "max_output_tokens", "max_output_tokens"
		}
		keys := slices.Sorted(maps.Keys(s.Extra))
		for i, k := range keys {
			if types.ExtraKeyReserved(k) {
				bad("%s.settings.extra.%s is set by hfy and cannot be set here", where, k)
			}
			for name, by := range owner {
				if strings.EqualFold(name, k) {
					bad("%s.settings.extra.%s clashes with settings.%s: set it there, not in extra", where, k, by)
				}
			}
			if j := slices.IndexFunc(keys[:i], func(o string) bool { return strings.EqualFold(o, k) }); j >= 0 {
				bad("%s.settings.extra: %s and %s differ only in case", where, keys[j], k)
			}
		}
	}

	if _, taken := c.MCP[BuiltinServer]; taken {
		bad("mcp.%s: the name is reserved for hfy's built-in tools", BuiltinServer)
	}
	for name, s := range c.MCP {
		if !nameRe.MatchString(name) || strings.Contains(name, "__") {
			bad("mcp.%s: names use [a-zA-Z0-9_-] and no double underscore", name)
		}
		for _, p := range s.Approval {
			if _, err := path.Match(p, ""); err != nil {
				bad("mcp.%s.approval: bad pattern %q", name, p)
			}
		}
		if s.UI != nil {
			for _, h := range s.UI.ImageHosts {
				if !hostPatternRe.MatchString(h) {
					bad(`mcp.%s.ui.image_hosts: %q must be a host like cdn.example.com or "*.example.com"`, name, h)
				}
			}
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

	if f := c.Files; f != nil {
		if len(f.Types) == 0 {
			bad("files.types is required")
		}
		for i, t := range f.Types {
			if !mimeRe.MatchString(t) {
				bad("files.types: %q must be a lowercase type/subtype like image/png (no wildcards or parameters)", t)
			} else if slices.Contains(f.Types[:i], t) {
				bad("files.types: %q is listed twice", t)
			}
		}
		if !f.URLs() && !f.Uploads() {
			bad("files needs allowed_hosts (URL attachments) or dsn (uploads)")
		}
		for i, h := range f.AllowedHosts {
			if !validAllowedHost(h) {
				bad(`files.allowed_hosts: %q must be a lowercase host like cdn.example.com, "*.example.com" or an IP (no scheme, port or path; IDN as punycode)`, h)
			} else if slices.Contains(f.AllowedHosts[:i], h) {
				bad("files.allowed_hosts: %q is listed twice", h)
			}
		}
		if f.MaxCount <= 0 {
			bad("files.max_count must be greater than 0")
		}
		if f.MaxBytes <= 0 {
			bad("files.max_bytes must be greater than 0")
		}
		if f.DSN != "" {
			if scheme, _, _ := strings.Cut(f.DSN, "://"); !slices.Contains([]string{"s3", "file"}, scheme) {
				bad("files.dsn must use s3:// or file:// (mem:// is for tests, injected directly)")
			} else if scheme == "s3" && !dsnHasRegion(f.DSN) {
				bad("files.dsn: an s3:// DSN must carry region=<region> in its query, e.g. s3://bucket?region=eu-west-1")
			}
		} else if f.PublicURL != "" {
			bad("files.public_url needs files.dsn (it is the download base for uploads)")
		}
		if f.UploadTTL <= 0 || f.UploadTTL > Duration(time.Hour) {
			bad("files.upload_ttl must be greater than 0 and at most 1h")
		}
		if f.DownloadTTL <= 0 || f.DownloadTTL > Duration(24*time.Hour) {
			bad("files.download_ttl must be greater than 0 and at most 24h")
		}
		if f.PublicURL != "" {
			if u, err := url.Parse(f.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				bad("files.public_url must be an http(s) URL with a host")
			}
		}
	}

	agents := c.Agents
	if agents[types.AgentMain] == nil {
		bad(`agents.main is required (the default agent: a grant without "agent" runs it)`)
	}
	for _, name := range slices.Sorted(maps.Keys(agents)) {
		a := agents[name]
		where := "agents." + name
		if name == types.AgentSummarizer || !nameRe.MatchString(name) || strings.Contains(name, "__") {
			bad("%s: name is reserved or invalid (use [a-zA-Z0-9_-], no double underscore)", where)
		}
		checkModel(where, a.Model)
		checkControls(where, a.Settings)
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
				if slices.Contains(coreNoApprovalTools, p) {
					bad("%s.approval.tools: %q is a core tool and never needs approval", where, p)
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
		checkControls(where, s.Settings)
		checkMCP(where, s.MCP)
		if strings.Contains(s.System, "{{") {
			bad("%s.system: sub-agents receive no vars, so {{...}} is not allowed", where)
		}
	}

	if s := c.Summarizer; s != nil {
		checkModel("summarizer", s.Model)
		checkControls("summarizer", s.Settings)
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
var BuiltinTools = []string{"tool_search", "now", "math_calc", "ask_user"}

// coreNoApprovalTools are the full names of the built-ins that never wait for
// approval (the agent decides the same at call time), so a pattern protecting
// only one of them can never work and is rejected at startup. hfy.ask_user is
// not one: it pauses on its own branch before approval ever applies.
var coreNoApprovalTools = []string{"hfy.now", "hfy.math_calc", "hfy.tool_search"}

// Main is the default agent: the one a grant that names none runs.
func (c *Config) Main() *Agent { return c.Agents[types.AgentMain] }

// AgentNamed returns the agent a grant picked ("" = main).
func (c *Config) AgentNamed(name string) (*Agent, bool) {
	if name == "" {
		name = types.AgentMain
	}
	a, ok := c.Agents[name]
	return a, ok
}

// SplitModel splits "<provider>/<model>" (the model part may contain slashes).
func SplitModel(model string) (provider, name string) {
	provider, name, _ = strings.Cut(model, "/")
	return
}
