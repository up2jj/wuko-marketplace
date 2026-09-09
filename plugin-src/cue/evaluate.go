package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	cueerrors "cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/parser"
)

const maxSafeInteger = 9007199254740991

const maxOutputSize = maxFrameSize - 4<<10

func evaluate(job workerJob) (json.RawMessage, error) {
	if err := validateImports(job.Source, job.Filename); err != nil {
		return nil, err
	}
	ctx := cuecontext.New()
	scope, err := cueScope(ctx, job.Context, job.Mode == workerModeValidate)
	if err != nil {
		return nil, err
	}
	program := ctx.CompileString(job.Source, cue.Filename(job.Filename), cue.Scope(scope))
	if err := program.Err(); err != nil {
		return nil, formatCUEError(err)
	}
	output := program.LookupPath(cue.MakePath(cue.Str("output")))
	if !output.Exists() {
		return nil, fmt.Errorf("%s: top-level field output is required", job.Filename)
	}
	if err := output.Validate(cue.Concrete(job.Mode == workerModeRun)); err != nil {
		return nil, formatCUEError(err)
	}
	if job.Mode == workerModeValidate {
		return nil, nil
	}
	data, err := output.MarshalJSON()
	if err != nil {
		return nil, formatCUEError(err)
	}
	if err := validateOutputSize(data); err != nil {
		return nil, err
	}
	if err := validateJSONNumbers(data); err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func validateOutputSize(data []byte) error {
	if len(data) > maxOutputSize {
		return fmt.Errorf("CUE output exceeds the plugin protocol limit")
	}
	return nil
}

func validateImports(source, filename string) error {
	file, err := parser.ParseFile(filename, source, parser.ImportsOnly)
	if err != nil {
		return formatCUEError(err)
	}
	for spec := range file.ImportSpecs() {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fmt.Errorf("%s: invalid import path %s", filename, spec.Path.Value)
		}
		if path == "tool" || strings.HasPrefix(path, "tool/") {
			return fmt.Errorf("%s: CUE tool packages are not supported: %q", filename, path)
		}
	}
	return nil
}

func cueScope(ctx *cue.Context, context stepContext, permissive bool) (cue.Value, error) {
	wuko := map[string]any{
		"inputs":       nonNilMap(context.Inputs),
		"vars":         nonNilMap(context.Vars),
		"env":          nonNilMap(context.Env),
		"steps":        nonNilMap(context.Steps),
		"dependencies": nonNilMap(context.Dependencies),
		"workflow": map[string]any{
			"name": context.WorkflowName,
			"dir":  context.WorkflowDir,
		},
		"run": map[string]any{"dir": context.RunDir},
		"step": map[string]any{
			"id": context.StepID, "attempt": context.Attempt,
			"max_attempts": context.MaxAttempts, "operation_id": context.OperationID,
		},
	}
	encoded, err := json.Marshal(map[string]any{"wuko": wuko})
	if err != nil {
		return cue.Value{}, fmt.Errorf("encoding Wuko context: %w", err)
	}
	if !permissive {
		scope := ctx.CompileBytes(encoded, cue.Filename("wuko-context.json"))
		if err := scope.Err(); err != nil {
			return cue.Value{}, formatCUEError(err)
		}
		return scope, nil
	}
	source := string(encoded) + ` & {
		wuko: {
			inputs: [string]: _
			vars: [string]: _
			env: [string]: _
			steps: [string]: [string]: _
			dependencies: [string]: _
		}
	}`
	scope := ctx.CompileString(source, cue.Filename("wuko-validation-context.cue"))
	if err := scope.Err(); err != nil {
		return cue.Value{}, formatCUEError(err)
	}
	return scope, nil
}

func nonNilMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func formatCUEError(err error) error {
	details := strings.TrimSpace(cueerrors.Details(err, nil))
	if details == "" {
		details = err.Error()
	}
	return fmt.Errorf("CUE evaluation failed: %s", details)
}

func validateJSONNumbers(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decoding CUE output: %w", err)
	}
	return walkJSONNumbers(value, "output")
}

func walkJSONNumbers(value any, path string) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if err := walkJSONNumbers(child, path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := walkJSONNumbers(child, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case json.Number:
		text := typed.String()
		if strings.ContainsAny(text, ".eE") {
			number, err := typed.Float64()
			if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
				return fmt.Errorf("%s contains a number that cannot cross the JSON protocol safely", path)
			}
			return nil
		}
		integer := new(big.Int)
		if _, ok := integer.SetString(text, 10); !ok {
			return fmt.Errorf("%s contains an invalid integer", path)
		}
		if new(big.Int).Abs(integer).Cmp(big.NewInt(maxSafeInteger)) > 0 {
			return fmt.Errorf("%s integer %s exceeds the JSON safe integer range", path, text)
		}
	}
	return nil
}
