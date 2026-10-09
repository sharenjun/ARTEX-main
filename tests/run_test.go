//go:build ignore

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, root, name, contents string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseArgs(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "norma/go.mod", "module example.com/norma\n")
	for _, tc := range []struct {
		name    string
		args    []string
		modules []string
		want    []string
	}{
		{"default", nil, []string{root}, []string{"./..."}},
		{"all", []string{"--all", "-run", "^$"}, []string{root, filepath.Join(root, "norma")}, []string{"./...", "-run", "^$"}},
		{"module", []string{"--module", "norma", "./llm", "-run", "Retry"}, []string{filepath.Join(root, "norma")}, []string{"./llm", "-run", "Retry"}},
		{"flags", []string{"-race", "-tags", "embedui", "-count=1"}, []string{root}, []string{"./...", "-race", "-tags", "embedui", "-count=1"}},
		{"packages", []string{"./db", "./server", "-run", "TestSide"}, []string{root}, []string{"./db", "./server", "-run", "TestSide"}},
		{"separator", []string{"--", "./config"}, []string{root}, []string{"./config"}},
		{"test args", []string{"-v", "-args", "custom"}, []string{root}, []string{"./...", "-v", "-args", "custom"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modules, args, err := parseArgs(root, tc.args)
			if err != nil || !reflect.DeepEqual(modules, tc.modules) || !reflect.DeepEqual(args, tc.want) {
				t.Fatalf("parseArgs() = %v, %v, %v; want %v, %v", modules, args, err, tc.modules, tc.want)
			}
		})
	}
	for _, args := range [][]string{
		{"--module"}, {"--module", "../outside"}, {"--module", root},
		{"--module", "missing"}, {"-overlay", "custom.json"}, {"-overlay=custom.json"},
	} {
		if _, _, err := parseArgs(root, args); err == nil {
			t.Errorf("parseArgs(%v) should fail", args)
		}
	}
}

func TestCollectTestsPreservesPackagesAndNestedModules(t *testing.T) {
	root := t.TempDir()
	source := writeFixture(t, root, "sample/tests/secret_test.go", strings.ReplaceAll(sourceHeader+"package sample\n", "\n", "\r\n"))
	writeFixture(t, root, "sample/tests/notes.txt", "not a Go test")
	writeFixture(t, root, "tests/run_test.go", sourceHeader+"package main\n")
	writeFixture(t, root, "norma/go.mod", "module example.com/norma\n")
	writeFixture(t, root, "norma/sample/tests/nested_test.go", sourceHeader+"package sample\n")
	writeFixture(t, root, "node_modules/dep/tests/dependency_test.go", sourceHeader+"package dep\n")
	replacements, err := collectTests(root, root, t.TempDir())
	if err != nil || len(replacements) != 1 {
		t.Fatalf("collectTests() = %v, %v", replacements, err)
	}
	staged := replacements[filepath.Join(root, "sample", "secret_test.go")]
	data, err := os.ReadFile(staged)
	if err != nil || string(data) != "package sample\n" {
		t.Fatalf("staged source = %q, %v", data, err)
	}
	original, _ := os.ReadFile(source)
	if !strings.Contains(string(original), "//go:build ignore") {
		t.Fatal("source was modified")
	}
	replacements, err = collectTests(root, filepath.Join(root, "norma"), t.TempDir())
	if err != nil || len(replacements) != 1 {
		t.Fatalf("nested module collectTests() = %v, %v", replacements, err)
	}
}

func TestCollectTestsRejectsCollisionAndMissingHeader(t *testing.T) {
	for _, collision := range []bool{false, true} {
		root := t.TempDir()
		contents := "package sample\n"
		if collision {
			contents = sourceHeader + contents
			writeFixture(t, root, "sample/secret_test.go", "original")
		}
		writeFixture(t, root, "sample/tests/secret_test.go", contents)
		if _, err := collectTests(root, root, t.TempDir()); err == nil {
			t.Fatalf("expected error, collision=%v", collision)
		}
	}
}

func TestOverlayRunsPrivateTestsInOriginalWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.com/overlayfixture\n\ngo 1.26\n")
	writeFixture(t, root, "sample/secret.go", "package sample\n\nfunc secret() int { return 42 }\n")
	writeFixture(t, root, "sample/testdata/message.txt", "fixture")
	writeFixture(t, root, "sample/tests/secret_test.go", sourceHeader+`package sample
import ("os"; "testing")
func TestSecret(t *testing.T) {
    if secret() != 42 { t.Fatal("private function inaccessible") }
    data, err := os.ReadFile("testdata/message.txt")
    if err != nil || string(data) != "fixture" { t.Fatalf("wrong working directory: %q %v", data, err) }
}
`)
	if err := testModule(root, root, []string{"./...", "-count=1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sample", "secret_test.go")); !os.IsNotExist(err) {
		t.Fatal("runner created a test file in the source directory")
	}
	// A failing test must propagate its nonzero exit status, not report success.
	writeFixture(t, root, "sample/tests/secret_test.go", sourceHeader+`package sample
import "testing"
func TestFail(t *testing.T) { t.Fatal("expected failure") }
`)
	if err := testModule(root, root, []string{"./...", "-count=1"}); err == nil {
		t.Fatal("runner swallowed a test failure")
	}
}

func TestRepositoryTestLayout(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	root := filepath.Dir(filepath.Dir(file))
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".codegraph", "node_modules", "vendor", ".next", "dist", "out":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		count++
		if filepath.Base(filepath.Dir(path)) != "tests" {
			t.Errorf("test must live in its package's tests directory: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(strings.ReplaceAll(string(data), "\r\n", "\n"), sourceHeader) {
			t.Errorf("test is missing the overlay source header: %s", path)
		}
		return nil
	})
	if err != nil || count == 0 {
		t.Fatalf("test layout scan: %d files, %v", count, err)
	}
}
