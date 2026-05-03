# Design Documentation

These documents describe the internal design of `go2bf`, a compiler
from Go to Brainfuck.

## Overview

- [Compiler Pipeline](pipeline.md) - the 6-stage compilation
  process from Go source to optimized Brainfuck

## Execution Model

- [Tape Layout](tape.md) - how the Brainfuck tape is organized:
  sentinels, highway markers, registers, temps, and the stack area
- [Stack](stack.md) - stride-3 slot layout, breadcrumb navigation,
  zero cell trick, and counter-walk for dynamic array access
- [Register Cache](cache.md) - 5-register LRU cache with
  dirty tracking to minimize stack round-trips

## Compilation

- [Lowering](lowering.md) - how Go constructs are translated to IR:
  function inlining, structs, arrays, slices, strings, multi-byte
  integers, defer, control flow, and `divmod` fusion
- [IR](ir.md) - the intermediate representation: node types, cell
  allocation, and peephole optimization
- [Code Generation](codegen.md) - BF generation for arithmetic,
  comparison, control flow, bitwise operations, and decimal printing

## Recursion

- [Recursion](recursion.md) - tail-call optimization, phase dispatch
  for general recursion, `noRetFlag`, conditional calls for if-branches,
  and execution traces
