package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
)

// Compile compiles Go source files to Brainfuck.
func Compile(filenames ...string) (string, error) {
	files, fset, err := Parse(filenames...)
	if err != nil {
		return "", err
	}
	return compile(files, fset, false)
}

// CompileSource compiles Go source code from a string to Brainfuck.
func CompileSource(src string) (string, error) {
	file, fset, err := ParseSource(src)
	if err != nil {
		return "", err
	}
	return compile([]*ast.File{file}, fset, false)
}

func compile(files []*ast.File, fset *token.FileSet, debug bool) (string, error) {
	info, err := Analyze(files, fset)
	if err != nil {
		return "", err
	}
	defer func(s int) { sentinelFwd = s }(sentinelFwd)
	maxSentinelFwd := sentinelFwd + maxSentinelBumps*highwayStride
	var prog *Program
	for {
		prog, err = Lower(info)
		if err == nil {
			break
		}
		if !errors.Is(err, errTooManyLocalsInRec) || sentinelFwd >= maxSentinelFwd {
			return "", err
		}
		sentinelFwd += highwayStride
	}
	if prog.CellsUsed-(sentinelFwd+1) > 255 {
		return "", fmt.Errorf(
			"too many variables: %d stack slots (max 255)", prog.CellsUsed-(sentinelFwd+1))
	}
	OptimizeIR(prog)
	return Optimize(Generate(prog, debug)), nil
}
