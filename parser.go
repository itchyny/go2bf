package main

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// Parse parses Go source files and returns their ASTs.
func Parse(filenames ...string) ([]*ast.File, *token.FileSet, error) {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, filename := range filenames {
		file, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, file)
	}
	return files, fset, nil
}

// ParseSource parses Go source code from a string.
func ParseSource(src string) (*ast.File, *token.FileSet, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "input.go", src, 0)
	if err != nil {
		return nil, nil, err
	}
	return file, fset, nil
}
