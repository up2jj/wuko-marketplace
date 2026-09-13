package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/build"
	"cuelang.org/go/cue/load"
	"cuelang.org/go/mod/modconfig"
	"cuelang.org/go/mod/modfile"
	"cuelang.org/go/mod/module"
)

const maxModuleInputSize = 10 << 20

type offlineRegistry struct{}

func (offlineRegistry) ModFile(_ context.Context, version module.Version) (*modfile.File, error) {
	return nil, fmt.Errorf("external CUE module dependencies are disabled: %s", version)
}

func (offlineRegistry) Fetch(_ context.Context, version module.Version) (module.SourceLoc, error) {
	return module.SourceLoc{}, fmt.Errorf("external CUE module dependencies are disabled: %s", version)
}

func (offlineRegistry) ModuleVersions(_ context.Context, modulePath string) ([]string, error) {
	return nil, fmt.Errorf("external CUE module dependencies are disabled: %s", modulePath)
}

var _ modconfig.Registry = offlineRegistry{}

type boundedFS struct {
	fsys fs.FS

	mu    sync.Mutex
	sizes map[string]int64
	total int64
}

func newBoundedFS(fsys fs.FS) *boundedFS {
	return &boundedFS{fsys: fsys, sizes: make(map[string]int64)}
}

func (b *boundedFS) Open(name string) (fs.File, error) {
	file, err := b.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return file, nil
	}
	if info.Size() > maxSourceSize {
		_ = file.Close()
		return nil, fmt.Errorf("CUE input file %q exceeds the 1 MiB limit", name)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.sizes[name]; exists {
		return file, nil
	}
	if b.total+info.Size() > maxModuleInputSize {
		_ = file.Close()
		return nil, fmt.Errorf("CUE module input exceeds the 10 MiB limit")
	}
	b.sizes[name] = info.Size()
	b.total += info.Size()
	return file, nil
}

func loadModuleProgram(ctx *cue.Context, scope cue.Value, configuration config, workflowDir string) (cue.Value, string, error) {
	if workflowDir == "" {
		return cue.Value{}, "", fmt.Errorf("workflow directory is required for file or package CUE input")
	}

	target, packageMode, err := moduleTarget(configuration)
	if err != nil {
		return cue.Value{}, "", err
	}
	root, err := os.OpenRoot(workflowDir)
	if err != nil {
		return cue.Value{}, "", fmt.Errorf("opening workflow directory: %w", err)
	}
	defer root.Close()

	virtualDir := path.Dir(target)
	arguments := []string{path.Base(target)}
	if packageMode {
		virtualDir = target
		arguments[0] = "."
	}
	if virtualDir == "." {
		virtualDir = "/"
	} else {
		virtualDir = "/" + virtualDir
	}
	loadConfig := &load.Config{
		Dir:      virtualDir,
		Env:      []string{},
		FS:       newBoundedFS(root.FS()),
		Registry: offlineRegistry{},
		FromFSPath: func(name string) string {
			name = strings.TrimPrefix(filepath.ToSlash(name), "/")
			if name == "" {
				return "."
			}
			return name
		},
	}
	instances := load.Instances(arguments, loadConfig)
	if len(instances) != 1 {
		return cue.Value{}, "", fmt.Errorf("CUE input %q resolved to %d packages, want exactly one", target, len(instances))
	}
	if err := validateLoadedImports(instances[0], make(map[*build.Instance]struct{})); err != nil {
		return cue.Value{}, "", err
	}
	program := ctx.BuildInstance(instances[0], cue.Scope(scope))
	if err := program.Err(); err != nil {
		if strings.Contains(err.Error(), "cannot find module providing package") {
			return cue.Value{}, "", fmt.Errorf("external CUE module dependencies are disabled: %w", err)
		}
		return cue.Value{}, "", formatCUEError(err)
	}
	return program, target, nil
}

func moduleTarget(configuration config) (target string, packageMode bool, err error) {
	target = configuration.File
	kind := "file"
	if configuration.Package != "" {
		target = configuration.Package
		kind = "package"
		packageMode = true
	}
	if filepath.IsAbs(target) {
		return "", false, fmt.Errorf("CUE %s must be relative to the workflow directory", kind)
	}
	for _, part := range strings.Split(filepath.ToSlash(target), "/") {
		if part == ".." {
			return "", false, fmt.Errorf("CUE %s path must not contain parent traversal", kind)
		}
	}
	clean := filepath.Clean(target)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("CUE %s path must not contain parent traversal", kind)
	}
	if !packageMode && filepath.Ext(clean) != ".cue" {
		return "", false, fmt.Errorf("CUE file must use the .cue extension")
	}
	return filepath.ToSlash(clean), packageMode, nil
}

func validateLoadedImports(instance *build.Instance, visited map[*build.Instance]struct{}) error {
	if _, exists := visited[instance]; exists {
		return nil
	}
	visited[instance] = struct{}{}
	for _, file := range instance.Files {
		if err := validateASTImports(file); err != nil {
			return err
		}
	}
	for _, imported := range instance.Imports {
		if err := validateLoadedImports(imported, visited); err != nil {
			return err
		}
	}
	return nil
}

func validateASTImports(file *ast.File) error {
	for spec := range file.ImportSpecs() {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fmt.Errorf("%s: invalid import path %s", file.Filename, spec.Path.Value)
		}
		if importPath == "tool" || strings.HasPrefix(importPath, "tool/") {
			return fmt.Errorf("%s: CUE tool packages are not supported: %q", file.Filename, importPath)
		}
	}
	return nil
}
