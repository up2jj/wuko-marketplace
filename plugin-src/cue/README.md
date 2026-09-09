# wuko-plugin-cue

`wuko-plugin-cue` implements Wuko plugin protocol v1 and contributes the `cue.eval` step. It evaluates one self-contained CUE source with a read-only snapshot of the Wuko runtime and returns the concrete top-level `output` value.

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

Use `file: policy.cue` instead of `source` for a workflow-relative CUE file. File access is confined to the workflow directory. Version 0.1.0 intentionally does not load CUE modules, sibling packages, remote sources, or tool tasks.

The injected `wuko` object exposes `inputs`, `vars`, `env`, `steps`, `dependencies`, `workflow.name`, `workflow.dir`, `run.dir`, and step attempt metadata. The snapshot is immutable and the step only returns `steps.<id>.value`; it cannot write workflow variables.

Run `just check`, `just build`, and `just release 0.1.0` from this directory. Import the release from the marketplace root with:

```sh
wuko marketplace plugin update cue ./plugin-src/cue
wuko marketplace build
```
