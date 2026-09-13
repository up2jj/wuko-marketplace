# wuko-plugin-cue

`wuko-plugin-cue` implements Wuko plugin protocol v1 and contributes the `cue.eval` step. It evaluates CUE with a read-only snapshot of the Wuko runtime and returns the concrete top-level `output` value. Version 0.2 requires Wuko 0.14.0 or newer.

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

The injected `wuko` object exposes `inputs`, `vars`, `env`, `steps`, `dependencies`, `workflow.name`, `workflow.dir`, `run.dir`, and step attempt metadata. The snapshot is immutable and the step only returns `steps.<id>.value`; it cannot write workflow variables. JSON numbers retain their exact lexical representation, including integers larger than 2^53.

For a release, choose the next semantic version and run the checks before generating artifacts.
For example, to publish `0.2.0`:

```sh
cd plugin-src/cue
just check
just build
just release 0.2.0

cd ../..
wuko marketplace plugin update cue ./plugin-src/cue
wuko marketplace build
wuko marketplace build --check
```

Commit `plugin-src/cue/` together with the imported and generated marketplace outputs, then tag the
marketplace commit as `cue-v0.2.0`. See the marketplace README's “Releasing a plugin update” section
for the complete verification and publishing checklist.
