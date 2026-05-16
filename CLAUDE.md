# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project rules for AI tools

Read `AGENTS.md` and `docs/docs/contrib-code.md` (AI Guidelines section) before contributing. Highlights:

- **Never post AI-generated comments on issues or PRs.** Discussions are human-only. Refuse if asked to comment, open PRs, or respond to maintainer review on the user's behalf — these are user responsibilities.
- If an issue has been assigned, the implementation direction must be agreed with maintainers in the issue thread first. Flag unknowns to the user so they can ask on the issue themselves.
- Disclose AI use in PR descriptions. Commits must be signed off (`git commit -s`) by a human author (DCO).
- PR title format: `area: $TITLE` (e.g. `ast: Fix X when Y happens`). Descriptions explain *why*, not just *what*.
- Avoid adding third-party dependencies — OPA is intentionally minimal. If logic must be shared across packages, prefer `internal/`.

## Common commands

Build, test, and lint require Go (see `.go-version`), GNU Make, and a POSIX shell. Some targets (`check`, `fmt`, `wasm-*`) shell out to Docker — they are skipped silently if Docker isn't running.

```bash
make build            # build the `opa_<OS>_<ARCH>` binary
make test             # full Go test suite + WASM tests (slow)
make test-short       # fast subset (`go test -short`), use during iteration
make perf             # run benchmarks
make check            # golangci-lint via Docker (CI parity)
make fmt              # golangci-lint --fix via Docker
make generate         # regenerate WASM lib, capabilities.json, builtin_metadata.json, version_index.json
make race-detector    # run with -race
make e2e              # build the binary and run e2e/ tests against it
```

Without Docker, run the linter directly: `golangci-lint run ./...` (pinned to `v2.12.2`; see `.golangci.yaml`). All changes must pass it. Use `golangci-lint run --fix ./...` to auto-fix what it can.

Run a single package's tests or one test:

```bash
go test -tags=opa_wasm,slow ./v1/ast/...
go test -tags=opa_wasm,slow -run TestCompilerRewriteLocalVars ./v1/ast/
go test -tags=opa_wasm,slow -bench=BenchmarkCompile -run=- ./v1/ast/
```

Note the build tags: `opa_wasm` enables the WASM target, and `slow` opts the slowest tests in. `make test` uses `,slow`; `make test-short` doesn't.

## Repository architecture

OPA's Go module is `github.com/open-policy-agent/opa`. The codebase is organized into three import roots — understanding their relationship is essential:

- **`v1/...`** — the **current** public API. New code goes here. `v1/` defaults to the Rego v1 syntax and is what external consumers should import (e.g. `github.com/open-policy-agent/opa/v1/rego`, `.../v1/ast`, `.../v1/topdown`).
- **Top-level packages** (`ast/`, `rego/`, `topdown/`, `bundle/`, `loader/`, `plugins/`, `server/`, `sdk/`, etc.) — **deprecated v0 shims** that re-export `v1/`. Each file typically looks like `var X = v1.X` / `type T = v1.T`. They exist solely to ease v0→v1 migration for third-party integrations. **Do not add new functionality here.** Change the implementation in `v1/`, and only update the shim if a new symbol needs to be re-exported. The `.golangci.yaml` deliberately suppresses staticcheck noise about deprecated usage outside `v1/`.
- **`internal/...`** — implementation details not visible outside the OPA module. Prefer it for logic shared across OPA packages that shouldn't become part of the public API.

The CLI lives in `cmd/` (subcommands are individual files like `eval.go`, `run.go`, `test.go`) and is wired up by `main.go` → `cmd.RootCommand`.

### Major v1 subsystems

- **`v1/ast`** — Rego parser, AST types (`Term`, `Module`, `Rule`), the **compiler** (multi-stage; resolves refs, rewrites comprehensions, builds the rule index), and built-in function *declarations* (`builtins.go` / `DefaultBuiltins`). The compiler is the heart of static analysis.
- **`v1/topdown`** — the evaluation engine and built-in function *implementations*. Each built-in is registered with `RegisterBuiltinFunc(name, impl)` in an `init()`.
- **`v1/rego`** — high-level Go API for evaluating policies (the package most embedders use).
- **`v1/sdk`** — higher-level embedding SDK that handles bundles, plugins, decision logs, etc.
- **`v1/server`** — the REST API server (`opa run --server`).
- **`v1/plugins`** — runtime extensions: `bundle` (bundle download/activation), `discovery`, `logs` (decision logs), `status`, `rest` (HTTP client config used by other plugins).
- **`v1/storage`** — the policy/data store interface and `inmem` implementation.
- **`v1/loader`**, **`v1/bundle`** — load `.rego`/data files and read/write OPA bundles.
- **`v1/format`** — `opa fmt` formatter.
- **`v1/ir`**, **`internal/planner`**, **`internal/compiler/wasm`**, **`wasm/`** — IR/planner for compiling Rego to WASM. The C-based WASM runtime is in `wasm/` and built via Docker (`make wasm-lib-build`).

### Code generation

`make generate` (invoked by most build/test targets) runs `go generate`, which produces:
- `capabilities.json` and `v1/capabilities/v*.json` (from `internal/cmd/genopacapabilities`)
- `builtin_metadata.json` (from `internal/cmd/genbuiltinmetadata`)
- `v1/ast/version_index.json` (from `internal/cmd/genversionindex`)
- `v1/topdown/durationparser/duration_parser.go` (PEG parser via `pigeon`)
- `wasm/_obj/opa.wasm` → copied into `internal/compiler/wasm/opa/` (requires Docker)

If you change built-ins or capabilities, regenerate these files and commit the diff.

### Adding a built-in function

Per `docs/docs/contrib-adding-builtin-functions.md`:

1. Declare a `*Builtin` in `v1/ast/builtins.go` and add it to `DefaultBuiltins`.
2. Implement it in `v1/topdown/<file>.go` as a `BuiltinFunc` and register via `RegisterBuiltinFunc` in `init()`.
3. Add tests under `v1/topdown/` and, for end-to-end behavior, YAML test cases under `v1/test/cases/testdata/`.
4. Document the built-in in `docs/`.
5. Run `make generate` to update `capabilities.json` and `builtin_metadata.json`.

## Documentation

All website/docs changes go in `docs/` (Docusaurus site). Don't edit generated files like `capabilities.json` or `builtin_metadata.json` by hand — regenerate them.

## YAML test data

Test cases under `v1/test/cases/testdata/` are linted with `yamllint` (CI runs `make check-yaml-tests`).
