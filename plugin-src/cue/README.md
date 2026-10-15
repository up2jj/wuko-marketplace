# wuko-plugin-cue

`wuko-plugin-cue` uses the Wuko Go SDK and plugin protocol v3 to contribute the `cue.eval` step. It evaluates CUE with a read-only snapshot of the Wuko runtime and returns the concrete top-level `output` value. Version 0.3 requires Wuko 0.16.0 or newer.

```yaml
- id: plan
  type: cue.eval
  with:
    source: |
      output: {
        name: "api-\(wuko.vars.environment)"
        replicas: wuko.vars.replicas
      }
```

Use exactly one input mode:

- `source` evaluates one self-contained inline file. Built-in CUE packages are available.
- `file` evaluates one workflow-relative `.cue` file and resolves imports from its local CUE module.
- `package` evaluates the single CUE package in a workflow-relative directory, unifying its files according to normal CUE module rules.

For example, `package: policy` loads a multi-file package from the workflow's `policy/` directory. Module and file access is confined to the workflow directory. Built-in and same-module imports are supported; registry dependencies, CUE tool packages, and local replacements outside the workflow are not. Each loaded file is limited to 1 MiB and the complete input to 10 MiB.

The injected `wuko` object exposes `inputs`, `vars`, `env`, `steps`, `dependencies`, `workflow.name`, `workflow.dir`, `run.dir`, and step attempt metadata. The snapshot is immutable and the step only returns `steps.<id>.value`; it cannot write workflow variables. Dynamic context numbers use the SDK's JSON representation; pass identifiers larger than 2^53 as strings when exact digits matter.

For a release, choose the next semantic version and run the checks before generating artifacts.
For example, to publish `0.3.0`:

```sh
wuko marketplace plugin release --version 0.3.0 ./plugin-src/cue
wuko marketplace build --check
```

Commit `plugin-src/cue/` together with the imported and generated marketplace outputs, then tag the
marketplace commit as `cue-v0.3.0`. See the marketplace README's “Releasing a plugin update” section
for the complete verification and publishing checklist.
