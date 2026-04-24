# Guidelines for go2bf

go2bf compiles Go source code to Brainfuck through a six-stage pipeline:
Parse -> Analyze -> Lower -> Optimize IR -> Generate BF -> Optimize BF.
See `docs/pipeline.md` for the full overview and `docs/` for detailed
design documentation.

## Architecture

`compiler.go` orchestrates the pipeline. `main.go` is the CLI
entry point. `interpreter.go` is the built-in BF interpreter.

The pipeline stages map to source files as follows:

| Stage | Files | Docs |
| ----- | ----- | ---- |
| Parse | `parser.go` | |
| Analyze | `analyzer.go` | |
| Lower | `lowerer.go`, `lowerer_rec.go`, `ir.go` | `lowering.md`, `ir.md` |
| Optimize IR | `ir_optimizer.go` | `ir.md` |
| Generate BF | `codegen.go` | `codegen.md`, `tape.md`, `cache.md`, `stack.md` |
| Optimize BF | `optimizer.go` | |

All docs are in the `docs/` directory. `pipeline.md` has the
full stage overview.

## Key Design Decisions

All values are `byte` (0-255). The BF tape uses a CPU-like model:
5 registers at positions 1,2,4,5,7 (interleaved with algo temps
at 3,6 for neighbor optimization), remaining algo temps at 9-23, phase temps
at 25-39, sentinels at 0 and 40, stride-3 stack starting at 41.
See `docs/tape.md` for the layout.

The register cache uses LRU eviction with round-robin allocation.
`storeToStack` is the most expensive operation. Optimizations
target reducing store count and navigation cost.

## Testing

Run all tests: `go test -timeout 10s ./...`

Run a single test: `go test -timeout 10s -run TestCompile/hello_world ./...`

Always use `-timeout 10s` to catch infinite loops from codegen bugs.

The test suite in `compiler_test.go` compiles Go snippets, runs
the BF output through the interpreter, and checks the result.

Testdata programs in `testdata/` serve as integration tests and
benchmarks. After any codegen change, compare BF output sizes
across all testdata programs:

```sh
for f in testdata/*.go; do
  echo "$(basename "$f"): $(go run . "$f" 2>/dev/null | wc -c)"
done
```

Check test coverage: `go test -coverprofile=coverage.out ./... &&
go tool cover -func=coverage.out | tail -1`

Keep test coverage high. When adding new code paths, add
corresponding test cases in `compiler_test.go`.

## Common Workflows

**Adding language features**: Add lowering in `lowerer.go` (or
`lowerer_rec.go` for recursive functions), add IR nodes in `ir.go`
if needed, add codegen in `codegen.go`, add tests in
`compiler_test.go`. Always update `README.md` (supported features,
limitations) and `docs/`.

**Bug investigation**: Use `-debug` to trace which IR operation
produces incorrect BF. Compare with a known-working BF interpreter.
The built-in interpreter may differ from external ones on EOF
behavior. Debug comments use `#` as a line prefix and must not
contain BF characters (`+-<>[].,`). The comment format:

- `r1`..`r5`: registers (positions 1,2,4,5,7)
- `r25`+: phase temp cells (positions 25-39)
- `%N`: stack slot N (e.g., `load r1 %3`, `store %0 r2`)
- `#N`: frame slot N (e.g., `load frame r28 #2`, `push frame #8`)
- `@B(rN)`: dynamic slot at base B indexed by register N
  (e.g., `load r2 @0(r1)`, `store @0(r7) r4`)

When investigating codegen bugs, write a standalone BF interpreter
with tape watchpoints to trace which positions change and when.
The built-in interpreter runs the generated BF correctly; use it
to confirm expected output. Compare BF output between working and
broken variants to find the divergence point.

**Performance optimization**: Measure BF output sizes before and
after changes across all testdata programs. Use `-debug` flag to
see annotated BF output with comments showing each IR operation.
Always update `README.md` and `docs/` when adding or changing
features and optimizations. Do not forget to update docs.

**Exploratory testing**: Write small Go programs that exercise edge
cases and run them with `go run . run /tmp/test.go`. Compare the
output with native Go before adding test cases. To convert go2bf
builtins to native Go equivalents:

- `putchar(x)` -> `os.Stdout.Write([]byte{x})`
- `print(x)` -> `fmt.Print(int(x))` (prints byte as decimal number)
- `println(a, b)` -> `fmt.Println(int(a), int(b))`
- `getchar()` ->
  `func getchar() byte { b := [1]byte{}; os.Stdin.Read(b[:]); return b[0] }`

Focus on recently added features, boundary conditions (overflow,
zero, empty), and combinations of features (e.g., struct fields
inside loops inside recursive functions).

## Code Style

- No Unicode characters in docs, comments, or code.
  Use ASCII `--` instead of em-dash, `->` instead of arrow.
- Functions prefixed with `gen` generate BF code for IR nodes.
  Functions prefixed with `emit` generate BF primitives (loops,
  conditionals). Functions prefixed with `lower` convert AST to IR.
- Tests use table-driven style with `{name, source, input, output}`
  tuples in `compiler_test.go`. Tests are organized by section
  (e.g., `// --- Arrays ---`). Place new tests in the correct section.
  Test names must be unique across all test functions.
  Always verify expected output with native Go before adding tests.
  Test source must compile and run correctly with native Go
  (using the builtin equivalents above). If the test uses go2bf-only
  features (byte pointers as slot indices), it cannot be verified
  natively -- avoid such patterns when possible.
  Prefer `print(" ")` over `putchar(' ')` for space separators.
- Group consecutive parameters of the same type:
  `func f(a, b int)` not `func f(a int, b int)`.
- Avoid speculative abstractions. Three similar lines are better
  than a premature helper.
- Use backticks for code identifiers in docs and comments
  (e.g., `divmod`, `IRDynLoad`, `allocCell`).
- Error messages: lowercase, no trailing period
  (e.g., `"unsupported expression: %T"`).
- Cross-references in `docs/` use markdown links
  (e.g., `[`stack.md`](stack.md)`).

## Known Limitations

See `README.md` for the full list. Additional compile-time
limitations not listed there are documented in `docs/recursion.md`.
