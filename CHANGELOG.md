# Changelog
## [v0.1.0](https://github.com/itchyny/go2bf/compare/v0.0.1..v0.1.0) (2026-06-12)
* support `uint16`, `uint32`, `uint64` integer types
* support array nesting up to 3 levels of bytes, ellipsis-sized arrays
* support slices with a dynamic heap allocator and in-place self-concat extension
* support string variables backed by `[]byte` slices
* support local `const`, `type`, and pointer-receiver methods including nested struct field writes
* support labeled `break`, `continue` statements
* support `goto` statement via state-machine dispatch
* support top-level `var`/`const` declarations of scalars, arrays, structs, and slices
* support general recursion with multi-byte parameters, bitwise operators
* support `init()` function called before `main()`
* reject zero-length arrays and integer literal overflow at compile time

## [v0.0.1](https://github.com/itchyny/go2bf/compare/0b860d4..v0.0.1) (2026-04-24)
* initial implementation
