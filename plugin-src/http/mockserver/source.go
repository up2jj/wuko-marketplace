package mockserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	wukoexpr "github.com/up2jj/wuko/expression"
	"github.com/up2jj/wuko/step"
	"gopkg.in/yaml.v3"
)

type compiledAssertion struct {
	config  assertionConfig
	program *vm.Program
}

type compiledPatch struct {
	config patchConfig
	value  *dynamicValue
}

type compiledExpectation struct {
	config     expectationConfig
	source     string
	program    *vm.Program
	assertions []compiledAssertion
	patches    []compiledPatch
	response   compiledResponse
	// readsBodyBase64 records that the defining file mentions a base64 body anywhere. Encoding one
	// costs a third copy of every uploaded byte per request, so the server only pays it when some
	// expectation can observe the result. A response-side body_base64 matches too, which merely
	// keeps the encoding rather than dropping one an expression still needs.
	readsBodyBase64 bool
	used            int
}

// loadMode separates the engine's up-front validation pass from the run. The two differ in what
// the step configuration holds: validation sees the workflow text as written, while the engine has
// already rendered every template by the time it builds the runner that serves traffic.
type loadMode int

const (
	loadRun loadMode = iota
	loadValidate
)

// containsTemplate reports configuration that only names a path once the engine renders it.
// Validation runs before any step produces output, so such a value cannot be resolved yet.
func containsTemplate(value string) bool { return strings.Contains(value, "{{") }

func (runner *Runner) load(ctx context.Context, request step.Request, renderer step.TemplateRenderer, mode loadMode) ([]*compiledExpectation, map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// Sources arrive rendered at run, so they are used as written. During validation a templated
	// source names no file yet and resolving the literal text would reject a perfectly valid
	// workflow, so that one pass waits for the run. The initial state is still checked either way.
	sources := runner.config.Expectations
	var expectations []*compiledExpectation
	if mode == loadRun || !slices.ContainsFunc(sources, containsTemplate) {
		files, err := resolveExpectationSources(ctx, request, sources)
		if err != nil {
			return nil, nil, err
		}
		expectations, err = loadExpectationFiles(ctx, request, files, renderer)
		if err != nil {
			return nil, nil, err
		}
	}
	state, err := runner.loadInitialState(request, renderer, mode)
	if err != nil {
		return nil, nil, err
	}
	return expectations, state, nil
}

func resolveExpectationSources(ctx context.Context, request step.Request, sources []string) ([]string, error) {
	if request.WorkflowDir == "" {
		return nil, fmt.Errorf("workflow directory is unavailable")
	}
	if request.WorkflowDirBorrowed {
		return nil, fmt.Errorf("expectation sources require a packaged action: the workflow directory belongs to the caller")
	}
	root, err := filepath.Abs(request.WorkflowDir)
	if err != nil {
		return nil, fmt.Errorf("resolving workflow directory: %w", err)
	}
	rootPhysical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolving workflow directory %s: %w", root, err)
	}
	seen := make(map[string]struct{})
	var files []string
	for index, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		matches, err := resolveExpectationSource(root, rootPhysical, source)
		if err != nil {
			return nil, fmt.Errorf("expectations[%d] %q: %w", index, source, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("expectations[%d] %q matched no YAML files", index, source)
		}
		slices.Sort(matches)
		for _, match := range matches {
			if _, exists := seen[match]; exists {
				continue
			}
			seen[match] = struct{}{}
			files = append(files, match)
		}
	}
	return files, nil
}

func resolveExpectationSource(root, rootPhysical, source string) ([]string, error) {
	if strings.TrimSpace(source) == "" {
		return nil, fmt.Errorf("source must not be empty")
	}
	converted := filepath.FromSlash(source)
	if filepath.IsAbs(converted) || filepath.VolumeName(converted) != "" {
		return nil, fmt.Errorf("source must be relative")
	}
	cleaned := filepath.Clean(converted)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("source must not escape the workflow package")
	}
	if strings.ContainsAny(source, `*?[{`) {
		pattern := filepath.ToSlash(source)
		if !doublestar.ValidatePattern(pattern) {
			return nil, fmt.Errorf("invalid glob pattern")
		}
		return resolveExpectationGlob(root, rootPhysical, pattern)
	}
	path := filepath.Join(root, cleaned)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("source must not be a symbolic link")
	}
	if info.Mode().IsRegular() {
		if !yamlExtension(path) {
			return nil, fmt.Errorf("file must use .yaml or .yml")
		}
		if err := ensureWithin(rootPhysical, path); err != nil {
			return nil, err
		}
		return []string{filepath.Clean(path)}, nil
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source must be a regular file, directory, or glob")
	}
	if err := ensureWithin(rootPhysical, path); err != nil {
		return nil, err
	}
	var matches []string
	err = filepath.WalkDir(path, func(candidate string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if yamlExtension(candidate) {
				return fmt.Errorf("expectation file %s must not be a symbolic link", candidate)
			}
			return nil
		}
		if !entry.Type().IsRegular() || !yamlExtension(candidate) {
			return nil
		}
		if err := ensureWithin(rootPhysical, candidate); err != nil {
			return err
		}
		matches = append(matches, filepath.Clean(candidate))
		return nil
	})
	return matches, err
}

func resolveExpectationGlob(root, rootPhysical, pattern string) ([]string, error) {
	if !doublestar.ValidatePattern(pattern) {
		return nil, fmt.Errorf("invalid glob pattern")
	}
	var matches []string
	err := doublestar.GlobWalk(os.DirFS(root), pattern, func(match string, entry fs.DirEntry) error {
		path := filepath.Join(root, filepath.FromSlash(match))
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 && yamlExtension(match) {
			return fmt.Errorf("expectation file %s must not be a symbolic link", path)
		}
		if !entry.Type().IsRegular() || !yamlExtension(match) {
			return nil
		}
		if err := ensureWithin(rootPhysical, path); err != nil {
			return err
		}
		matches = append(matches, filepath.Clean(path))
		return nil
	}, doublestar.WithNoFollow(), doublestar.WithFailOnIOErrors())
	return matches, err
}

func ensureWithin(rootPhysical, path string) error {
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(rootPhysical, physical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %s escapes the workflow package", path)
	}
	return nil
}

func yamlExtension(path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	return extension == ".yaml" || extension == ".yml"
}

func loadExpectationFiles(ctx context.Context, request step.Request, files []string, renderer step.TemplateRenderer) ([]*compiledExpectation, error) {
	shape := expressionShape()
	seen := make(map[string]string)
	var result []*compiledExpectation
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading expectation file %s: %w", path, err)
		}
		var file expectationFile
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&file); err != nil {
			return nil, fmt.Errorf("decoding expectation file %s: %w", path, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err == nil {
			return nil, fmt.Errorf("decoding expectation file %s: multiple YAML documents are not supported", path)
		} else if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("decoding expectation file %s: %w", path, err)
		}
		readsBodyBase64 := bytes.Contains(data, []byte("body_base64"))
		if file.Version != 1 {
			return nil, fmt.Errorf("expectation file %s version must be 1", path)
		}
		if len(file.Expectations) == 0 {
			return nil, fmt.Errorf("expectation file %s must contain expectations", path)
		}
		for index, config := range file.Expectations {
			if strings.TrimSpace(config.Name) == "" {
				return nil, fmt.Errorf("expectation file %s item %d name is required", path, index)
			}
			if previous, exists := seen[config.Name]; exists {
				return nil, fmt.Errorf("expectation name %q is duplicated in %s and %s", config.Name, previous, path)
			}
			seen[config.Name] = path
			compiled, err := compileExpectation(request, path, config, file.Templates, renderer, shape)
			if err != nil {
				return nil, fmt.Errorf("expectation %q in %s: %w", config.Name, path, err)
			}
			// A body_file is compiled from a file this scan never saw, so check the text it pulled in
			// as well; otherwise a template there reads an empty body_base64 with no error.
			compiled.readsBodyBase64 = readsBodyBase64 || strings.Contains(compiled.response.body.text, "body_base64")
			result = append(result, compiled)
		}
	}
	return result, nil
}

func compileExpectation(request step.Request, source string, config expectationConfig, templates responseTemplates, renderer step.TemplateRenderer, shape map[string]any) (*compiledExpectation, error) {
	if strings.TrimSpace(config.When) == "" {
		return nil, fmt.Errorf("when is required")
	}
	if config.Times != nil && *config.Times <= 0 {
		return nil, fmt.Errorf("times must be positive when set")
	}
	program, err := wukoexpr.Compile(config.When, expr.Env(shape), expr.AllowUndefinedVariables(), expr.AsBool())
	if err != nil {
		return nil, fmt.Errorf("compiling when: %w", err)
	}
	assertionNames := make(map[string]struct{})
	assertions := make([]compiledAssertion, len(config.Assertions))
	for index, assertion := range config.Assertions {
		if strings.TrimSpace(assertion.Name) == "" || strings.TrimSpace(assertion.Expr) == "" {
			return nil, fmt.Errorf("assertions[%d] name and expr are required", index)
		}
		if _, exists := assertionNames[assertion.Name]; exists {
			return nil, fmt.Errorf("assertion name %q is duplicated", assertion.Name)
		}
		assertionNames[assertion.Name] = struct{}{}
		compiled, err := wukoexpr.Compile(assertion.Expr, expr.Env(shape), expr.AllowUndefinedVariables(), expr.AsBool())
		if err != nil {
			return nil, fmt.Errorf("compiling assertion %q: %w", assertion.Name, err)
		}
		if assertion.Message != "" && renderer != nil {
			if err := renderer.ValidateContent(assertion.Message); err != nil {
				return nil, fmt.Errorf("assertion %q message: %w", assertion.Name, err)
			}
		}
		assertions[index] = compiledAssertion{config: assertion, program: compiled}
	}
	patches := make([]compiledPatch, len(config.Update))
	for index, patch := range config.Update {
		if patch.Op != "set" && patch.Op != "append" && patch.Op != "remove" {
			return nil, fmt.Errorf("update[%d] op must be set, append, or remove", index)
		}
		if patch.Path == "" {
			return nil, fmt.Errorf("update[%d] path is required", index)
		}
		if renderer != nil {
			if err := renderer.ValidateContent(patch.Path); err != nil {
				return nil, fmt.Errorf("update[%d] path: %w", index, err)
			}
		}
		if patch.Op != "remove" && patch.Value == nil {
			return nil, fmt.Errorf("update[%d] value is required", index)
		}
		value, err := compileDynamic(patch.Value, shape, renderer)
		if err != nil {
			return nil, fmt.Errorf("update[%d] value: %w", index, err)
		}
		patches[index] = compiledPatch{config: patch, value: value}
	}
	response, err := compileResponse(request, source, config.Respond, templates, shape, renderer)
	if err != nil {
		return nil, err
	}
	return &compiledExpectation{config: config, source: source, program: program, assertions: assertions, patches: patches, response: response}, nil
}

func expressionShape() map[string]any {
	return step.ExpressionEnvironmentShape(map[string]any{
		"request": requestValue{}, "state": map[string]any{},
		"route": func(string) (bool, error) { return false, nil }, "header": func(string) string { return "" },
		"query": func(string) string { return "" }, "form": func(string) string { return "" },
		"jsonBody": func() (any, error) { return nil, errors.New("request JSON unavailable") },
	})
}

func validateListen(address string) error {
	if strings.Contains(address, "{{") {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen must be a host:port address: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen must use a loopback address")
	}
	return nil
}
