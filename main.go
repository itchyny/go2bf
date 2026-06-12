package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
)

const name = "go2bf"

const version = "0.1.0"

var revision = "HEAD"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if err == flag.ErrHelp {
			return
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fs.SetOutput(stdout)
		fmt.Fprintf(stdout, `%[1]s - compile Go to Brainfuck

Version: %s (rev: %s/%s)

Synopsis:
  %% %[1]s [-debug] [file.go ...|-]
  %% %[1]s run [file.go ...|-]

Options:
`, name, version, revision, runtime.Version())
		fs.PrintDefaults()
	}
	debug := fs.Bool("debug", false, "emit debug comments")
	width := fs.Int("width", 80, "line width for output formatting")
	showVersion := fs.Bool("version", false, "print version")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Fprintf(stdout, "%s %s (rev: %s)\n", name, version, revision)
		return nil
	}

	args = fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return nil
	}

	cmd := args[0]
	switch {
	case cmd == "run" && len(args) >= 2:
		files, fset, err := parseFiles(args[1:], stdin)
		if err != nil {
			return fmt.Errorf("compile error: %v", err)
		}
		code, err := compile(files, fset, false)
		if err != nil {
			return fmt.Errorf("compile error: %v", err)
		}
		if err := Interpret(code, stdin, stdout); err != nil {
			return fmt.Errorf("runtime error: %v", err)
		}
	case cmd != "run":
		files, fset, err := parseFiles(args, stdin)
		if err != nil {
			return fmt.Errorf("compile error: %v", err)
		}
		code, err := compile(files, fset, *debug)
		if err != nil {
			return fmt.Errorf("compile error: %v", err)
		}
		if *debug {
			fmt.Fprint(stdout, code)
		} else {
			fmt.Fprintln(stdout, formatBF(code, *width))
		}
	default:
		fs.Usage()
	}
	return nil
}

// parseFiles parses Go source files. A filename of "-" reads from stdin.
func parseFiles(filenames []string, stdin io.Reader) ([]*ast.File, *token.FileSet, error) {
	if !slices.Contains(filenames, "-") {
		return Parse(filenames...)
	}
	src, err := io.ReadAll(stdin)
	if err != nil {
		return nil, nil, fmt.Errorf("reading stdin: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, filename := range filenames {
		var file *ast.File
		if filename == "-" {
			file, err = parser.ParseFile(fset, "stdin.go", src, 0)
		} else {
			file, err = parser.ParseFile(fset, filename, nil, 0)
		}
		if err != nil {
			return nil, nil, err
		}
		files = append(files, file)
	}
	return files, fset, nil
}

func formatBF(code string, width int) string {
	if width <= 0 {
		return code
	}
	var buf strings.Builder
	buf.Grow(len(code) + len(code)/width + 1)
	for i, c := range code {
		if i > 0 && i%width == 0 {
			buf.WriteByte('\n')
		}
		buf.WriteRune(c)
	}
	return buf.String()
}
