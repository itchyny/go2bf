package main

import (
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		stdin   string
		want    string
		wantErr string
	}{
		{
			"compile file",
			[]string{"testdata/hello.go"},
			"",
			"[-]",
			"",
		},
		{
			"compile file with debug",
			[]string{"-debug", "testdata/hello.go"},
			"",
			"putc r1",
			"",
		},
		{
			"run hello",
			[]string{"run", "testdata/hello.go"},
			"",
			"Hello, World!\n",
			"",
		},
		{
			"version flag",
			[]string{"-version"},
			"",
			name,
			"",
		},
		{
			"run from stdin",
			[]string{"run", "-"},
			"package main\nfunc main() { print(42) }\n",
			"42",
			"",
		},
		{
			"compile from stdin",
			[]string{"-"},
			"package main\nfunc main() { print(42) }\n",
			"[-]",
			"",
		},
		{
			"compile error",
			[]string{"run", "testdata/nonexistent.go"},
			"",
			"",
			"compile error",
		},
		{
			"compile syntax error from stdin",
			[]string{"run", "-"},
			"not valid go",
			"",
			"compile error",
		},
		{
			"run with no file",
			[]string{"run"},
			"",
			name + " - compile Go to Brainfuck",
			"",
		},
		{
			"invalid flag",
			[]string{"-invalid"},
			"",
			"",
			"flag provided but not defined",
		},
		{
			"width zero",
			[]string{"-width", "0", "testdata/hello.go"},
			"",
			"[-]",
			"",
		},
		{
			"no args shows usage",
			[]string{},
			"",
			name + " - compile Go to Brainfuck",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			stdin := strings.NewReader(tt.stdin)
			err := run(tt.args, stdin, &stdout, &stderr)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.want != "" && !strings.Contains(stdout.String(), tt.want) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.want)
			}
		})
	}
}
