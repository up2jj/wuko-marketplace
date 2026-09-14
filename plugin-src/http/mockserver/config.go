// Package mockserver implements a lifecycle-managed HTTP mock server.
package mockserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/up2jj/wuko/step"
	"gopkg.in/yaml.v3"
)

const defaultMaxBodyBytes int64 = 10 << 20

type Config struct {
	Listen           string     `yaml:"listen,omitempty"`
	TLS              bool       `yaml:"tls,omitempty"`
	MaxBodyBytes     int64      `yaml:"max_body_bytes,omitempty"`
	Expectations     []string   `yaml:"expectations"`
	InitialState     any        `yaml:"initial_state,omitempty"`
	InitialStateFile string     `yaml:"initial_state_file,omitempty"`
	Log              *LogConfig `yaml:"log,omitempty"`
}

type LogConfig struct {
	Destination   string   `yaml:"destination"`
	Path          string   `yaml:"path,omitempty"`
	Overwrite     bool     `yaml:"overwrite,omitempty"`
	IncludeBodies bool     `yaml:"include_bodies,omitempty"`
	RedactHeaders []string `yaml:"redact_headers,omitempty"`
}

type expectationFile struct {
	Version      int                 `yaml:"version"`
	Templates    responseTemplates   `yaml:"templates,omitempty"`
	Expectations []expectationConfig `yaml:"expectations"`
}

type responseTemplates struct {
	Headers map[string]map[string]stringList `yaml:"headers,omitempty"`
	Bodies  map[string]bodySpec              `yaml:"bodies,omitempty"`
}

type expectationConfig struct {
	Name       string            `yaml:"name"`
	When       string            `yaml:"when"`
	Times      *int              `yaml:"times,omitempty"`
	Assertions []assertionConfig `yaml:"assertions,omitempty"`
	Update     []patchConfig     `yaml:"update,omitempty"`
	Respond    responseSpec      `yaml:"respond"`
}

type assertionConfig struct {
	Name    string `yaml:"name"`
	Expr    string `yaml:"expr"`
	Message string `yaml:"message,omitempty"`
}

type patchConfig struct {
	Op    string `yaml:"op"`
	Path  string `yaml:"path"`
	Value any    `yaml:"value,omitempty"`
}

type responseSpec struct {
	defined         bool
	Status          int                   `yaml:"status,omitempty"`
	HeadersTemplate string                `yaml:"headers_template,omitempty"`
	BodyTemplate    string                `yaml:"body_template,omitempty"`
	Headers         map[string]stringList `yaml:"headers,omitempty"`
	bodySpec        `yaml:",inline"`
}

type bodySpec struct {
	Body        *string `yaml:"body,omitempty"`
	LiteralBody *string `yaml:"literal_body,omitempty"`
	JSON        any     `yaml:"json,omitempty"`
	BodyFile    string  `yaml:"body_file,omitempty"`
	BodyBase64  *string `yaml:"body_base64,omitempty"`
	hasJSON     bool
}

type stringList []string

func (values *stringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return fmt.Errorf("header value must be a string or list of strings")
		}
		*values = []string{node.Value}
		return nil
	case yaml.SequenceNode:
		result := make([]string, len(node.Content))
		for index, item := range node.Content {
			if item.Tag != "!!str" {
				return fmt.Errorf("header value item %d must be a string", index)
			}
			result[index] = item.Value
		}
		*values = result
		return nil
	default:
		return fmt.Errorf("header value must be a string or list of strings")
	}
}

func (body *bodySpec) UnmarshalYAML(node *yaml.Node) error {
	type plain bodySpec
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*body = bodySpec(value)
	body.hasJSON = mappingHasKey(node, "json")
	return validateMappingKeys(node, "body", map[string]struct{}{
		"body": {}, "literal_body": {}, "json": {}, "body_file": {}, "body_base64": {},
	})
}

func (response *responseSpec) UnmarshalYAML(node *yaml.Node) error {
	response.defined = true
	if node.Kind == yaml.ScalarNode {
		if node.Tag != "!!str" {
			return fmt.Errorf("respond must be a string or object")
		}
		value := node.Value
		response.Body = &value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("respond must be a string or object")
	}
	if err := validateMappingKeys(node, "respond", map[string]struct{}{
		"status": {}, "headers_template": {}, "body_template": {}, "headers": {},
		"body": {}, "literal_body": {}, "json": {}, "body_file": {}, "body_base64": {},
	}); err != nil {
		return err
	}
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index].Value, node.Content[index+1]
		var target any
		switch key {
		case "status":
			target = &response.Status
		case "headers_template":
			target = &response.HeadersTemplate
		case "body_template":
			target = &response.BodyTemplate
		case "headers":
			target = &response.Headers
		case "body":
			target = &response.Body
		case "literal_body":
			target = &response.LiteralBody
		case "json":
			response.hasJSON = true
			target = &response.JSON
		case "body_file":
			target = &response.BodyFile
		case "body_base64":
			target = &response.BodyBase64
		}
		if err := value.Decode(target); err != nil {
			return fmt.Errorf("field %s: %w", key, err)
		}
	}
	return nil
}

func validateMappingKeys(node *yaml.Node, name string, allowed map[string]struct{}) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("%s must be an object", name)
	}
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index].Value
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("field %s not found in %s", key, name)
		}
	}
	return nil
}

func mappingHasKey(node *yaml.Node, key string) bool {
	if node.Kind != yaml.MappingNode {
		return false
	}
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return true
		}
	}
	return false
}

type Runner struct{ config Config }

func Register(registry *step.Registry) error { return registry.Register("http.mock_server", New) }

// ServiceOptions exposes the native service policy to the protocol adapter.
func (runner *Runner) ServiceOptions() (string, step.ServiceOptions) {
	return "mock_server", step.ServiceOptions{FailFast: true}
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
		config.MaxBodyBytes = defaultMaxBodyBytes
	}
	if len(config.Expectations) == 0 {
		return nil, fmt.Errorf("expectations must contain at least one source")
	}
	if config.MaxBodyBytes <= 0 {
		return nil, fmt.Errorf("max_body_bytes must be positive")
	}
	if config.InitialState != nil && strings.TrimSpace(config.InitialStateFile) != "" {
		return nil, fmt.Errorf("initial_state and initial_state_file are mutually exclusive")
	}
	if err := validateListen(config.Listen); err != nil {
		return nil, err
	}
	if err := validateLogConfig(config.Log); err != nil {
		return nil, err
	}
	return &Runner{config: config}, nil
}

func (runner *Runner) Validate(ctx context.Context, request step.Request) error {
	_, _, err := runner.load(ctx, request, request.TemplateRenderer, loadValidate)
	return err
}

func validateLogConfig(config *LogConfig) error {
	if config == nil || strings.Contains(config.Destination, "{{") {
		return nil
	}
	switch config.Destination {
	case "stdout":
		if config.Path != "" || config.Overwrite {
			return fmt.Errorf("log path and overwrite are only valid for file destination")
		}
	case "file":
		if strings.TrimSpace(config.Path) == "" {
			return fmt.Errorf("log path is required for file destination")
		}
	default:
		return fmt.Errorf("log destination must be stdout or file")
	}
	return nil
}
