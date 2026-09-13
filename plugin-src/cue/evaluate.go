package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	cueerrors "cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/parser"
)

const maxOutputSize = maxFrameSize - 4<<10

func evaluate(job workerJob) (json.RawMessage, error) {
	ctx := cuecontext.New()
	scope, err := cueScope(ctx, job.Context, job.Mode == workerModeValidate)
	if err != nil {
		return nil, err
	}
	var program cue.Value
	filename := "inline.cue"
	if job.Configuration.Source != "" {
		if len(job.Configuration.Source) > maxSourceSize {
			return nil, fmt.Errorf("inline CUE source exceeds the 1 MiB limit")
		}
		if err := validateImports(job.Configuration.Source, filename); err != nil {
			return nil, err
		}
		program = ctx.CompileString(job.Configuration.Source, cue.Filename(filename), cue.Scope(scope))
		if err := program.Err(); err != nil {
			return nil, formatCUEError(err)
		}
	} else {
		program, filename, err = loadModuleProgram(ctx, scope, job.Configuration, job.Context.WorkflowDir)
		if err != nil {
			return nil, err
		}
	}
	output := program.LookupPath(cue.MakePath(cue.Str("output")))
	if !output.Exists() {
		return nil, fmt.Errorf("%s: top-level field output is required", filename)
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
	return validateASTImports(file)
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
