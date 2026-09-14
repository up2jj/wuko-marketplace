// Package forwardproxy implements a lifecycle-managed intercepting HTTP proxy.
package forwardproxy

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	wukoexpr "github.com/up2jj/wuko/expression"
	"github.com/up2jj/wuko/step"
	"gopkg.in/yaml.v3"
)

const defaultMaxBodyBytes int64 = 10 << 20

type byteSize int64

func (size *byteSize) UnmarshalYAML(node *yaml.Node) error {
	text := strings.TrimSpace(node.Value)
	if text == "" {
		return fmt.Errorf("size must not be empty")
	}
	// The engine builds a runner once against the unrendered configuration to validate the
	// workflow, and again against the rendered one to run it. A templated size is only a number
	// on the second pass, so leave it unset here and let the default stand for validation.
	if templated(text) {
		*size = 0
		return nil
	}
	multiplier := int64(1)
	for suffix, value := range map[string]int64{
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30,
		"KB": 1000, "MB": 1000 * 1000, "GB": 1000 * 1000 * 1000,
	} {
		if strings.HasSuffix(text, suffix) {
			text = strings.TrimSpace(strings.TrimSuffix(text, suffix))
			multiplier = value
			break
		}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value <= 0 || value > (1<<63-1)/multiplier {
		return fmt.Errorf("size must be a positive byte count such as 10485760 or 10MiB")
	}
	*size = byteSize(value * multiplier)
	return nil
}

type Config struct {
	Listen       string     `yaml:"listen,omitempty"`
	KeepAlive    bool       `yaml:"keep_alive,omitempty"`
	MaxBodyBytes byteSize   `yaml:"max_body_bytes,omitempty"`
	Rules        []Rule     `yaml:"rules,omitempty"`
	Lua          *LuaConfig `yaml:"lua,omitempty"`
	Log          *LogConfig `yaml:"log,omitempty"`
}

type Rule struct {
	Name    string  `yaml:"name,omitempty"`
	When    string  `yaml:"when"`
	Rewrite Rewrite `yaml:"rewrite,omitempty"`
	Stop    bool    `yaml:"stop,omitempty"`
}

type Rewrite struct {
	URL        string       `yaml:"url,omitempty"`
	Scheme     string       `yaml:"scheme,omitempty"`
	Host       string       `yaml:"host,omitempty"`
	Path       string       `yaml:"path,omitempty"`
	Method     string       `yaml:"method,omitempty"`
	Headers    ValueChanges `yaml:"headers,omitempty"`
	Query      ValueChanges `yaml:"query,omitempty"`
	Body       *string      `yaml:"body,omitempty"`
	BodyBase64 *string      `yaml:"body_base64,omitempty"`
}

type ValueChanges struct {
	Set    map[string][]string `yaml:"set,omitempty"`
	Add    map[string][]string `yaml:"add,omitempty"`
	Remove []string            `yaml:"remove,omitempty"`
}

type LuaConfig struct {
	File   string `yaml:"file,omitempty"`
	Source string `yaml:"source,omitempty"`
}

type LogConfig struct {
	Destination   string   `yaml:"destination"`
	Path          string   `yaml:"path,omitempty"`
	Overwrite     bool     `yaml:"overwrite,omitempty"`
	IncludeBodies bool     `yaml:"include_bodies,omitempty"`
	RedactHeaders []string `yaml:"redact_headers,omitempty"`
}

type compiledRule struct {
	rule    Rule
	program *vm.Program
}

type Runner struct {
	config Config
	rules  []compiledRule
}

func Register(registry *step.Registry) error { return registry.Register("http.forward_proxy", New) }

// ServiceOptions exposes the native service policy to the protocol adapter.
func (runner *Runner) ServiceOptions() (string, step.ServiceOptions) {
	return "forward_proxy", step.ServiceOptions{KeepAlive: runner.config.KeepAlive, FailFast: true}
}

func New(raw map[string]any) (step.Runner, error) {
	var config Config
	if err := step.DecodeConfig(raw, &config); err != nil {
		return nil, err
	}
	if config.Listen == "" {
		config.Listen = "127.0.0.1:0"
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = byteSize(defaultMaxBodyBytes)
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	rules := make([]compiledRule, len(config.Rules))
	shape := step.ExpressionEnvironmentShape(map[string]any{"original": requestValue{}})
	for index, rule := range config.Rules {
		program, err := wukoexpr.Compile(rule.When, expr.Env(shape), expr.AllowUndefinedVariables(), expr.AsBool())
		if err != nil {
			return nil, fmt.Errorf("compiling rule %q: %w", ruleName(rule, index), err)
		}
		rules[index] = compiledRule{rule: rule, program: program}
	}
	return &Runner{config: config, rules: rules}, nil
}

func validateConfig(config Config) error {
	if err := validateListen(config.Listen); err != nil {
		return err
	}
	for index, rule := range config.Rules {
		name := ruleName(rule, index)
		if strings.TrimSpace(rule.When) == "" {
			return fmt.Errorf("rule %q when is required", name)
		}
		if rule.Rewrite.Body != nil && rule.Rewrite.BodyBase64 != nil {
			return fmt.Errorf("rule %q body and body_base64 are mutually exclusive", name)
		}
		if rule.Rewrite.URL != "" && (rule.Rewrite.Scheme != "" || rule.Rewrite.Host != "" || rule.Rewrite.Path != "") {
			return fmt.Errorf("rule %q url is mutually exclusive with scheme, host, and path", name)
		}
	}
	if config.Lua != nil && (strings.TrimSpace(config.Lua.File) == "") == (strings.TrimSpace(config.Lua.Source) == "") {
		return fmt.Errorf("lua requires exactly one of file or source")
	}
	if config.Log == nil || templated(config.Log.Destination) {
		return nil
	}
	switch config.Log.Destination {
	case "stdout":
		if config.Log.Path != "" {
			return fmt.Errorf("log path is only valid for file destination")
		}
		if config.Log.Overwrite {
			return fmt.Errorf("log overwrite is only valid for file destination")
		}
	case "file":
		if strings.TrimSpace(config.Log.Path) == "" {
			return fmt.Errorf("log path is required for file destination")
		}
	default:
		return fmt.Errorf("log destination must be stdout or file")
	}
	return nil
}

// templated reports that a value still carries a Go template, which the engine renders only on the
// pass that builds the runner it actually runs. Value checks must skip such text so a workflow that
// computes its configuration is not rejected while it is being validated.
func templated(value string) bool { return strings.Contains(value, "{{") }

func ruleName(rule Rule, index int) string {
	if rule.Name != "" {
		return rule.Name
	}
	return strconv.Itoa(index + 1)
}
