//go:build ignore

// Run package-local tests stored in each package's tests directory.
// Usage: go run ./tests/run.go [--all | --module norma] [go test arguments...]
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const sourceHeader = "//go:build ignore\n\n"

type overlay struct {
	Replace map[string]string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return errors.New("cannot locate tests/run.go")
	}
	root := filepath.Dir(filepath.Dir(file))
	modules, testArgs, err := parseArgs(root, args)
	if err != nil {
		return err
	}
	if modules == nil {
		return nil // --help
	}
	var failures []error
	for _, module := range modules {
		if err := testModule(root, module, testArgs); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func parseArgs(root string, args []string) ([]string, []string, error) {
	modules := []string{root}
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h":
			fmt.Println("Usage: go run ./tests/run.go [--all | --module norma] [go test arguments...]")
			fmt.Println("Examples: --all; --module norma ./llm -run TestRetry; -race ./db ./server")
			return nil, nil, nil
		case "--all":
			modules = append(modules, filepath.Join(root, "norma"))
			args = args[1:]
		case "--module":
			if len(args) < 2 {
				return nil, nil, errors.New("--module requires a repository-relative module path")
			}
			if filepath.IsAbs(args[1]) {
				return nil, nil, errors.New("module path must be repository-relative")
			}
			module := filepath.Join(root, args[1])
			rel, err := filepath.Rel(root, module)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, nil, errors.New("module must be inside the repository")
			}
			if _, err := os.Stat(filepath.Join(module, "go.mod")); err != nil {
				return nil, nil, fmt.Errorf("invalid module %q: %w", args[1], err)
			}
			modules = []string{module}
			args = args[2:]
		}
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	// With only flags, go test defaults to the module root, which has no Go files.
	// Supply ./... unless the caller selected packages (before -args).
	hasPackage := false
	valueFlags := map[string]bool{
		"-run": true, "-skip": true, "-bench": true, "-benchtime": true,
		"-count": true, "-timeout": true, "-parallel": true, "-cpu": true,
		"-p": true, "-tags": true, "-coverpkg": true, "-coverprofile": true,
		"-covermode": true, "-gcflags": true, "-ldflags": true, "-asmflags": true,
		"-mod": true, "-modfile": true, "-vet": true, "-exec": true,
		"-outputdir": true, "-o": true, "-shuffle": true,
		"-blockprofile": true, "-blockprofilerate": true, "-cpuprofile": true,
		"-memprofile": true, "-memprofilerate": true, "-mutexprofile": true,
		"-mutexprofilefraction": true, "-trace": true, "-fullpath": false,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-args" {
			break
		}
		if arg == "-overlay" || strings.HasPrefix(arg, "-overlay=") {
			return nil, nil, errors.New("-overlay is managed by this runner")
		}
		if !strings.HasPrefix(arg, "-") {
			hasPackage = true
		} else if valueFlags[arg] {
			i++
		}
	}
	if !hasPackage {
		args = append([]string{"./..."}, args...)
	}
	return modules, args, nil
}

func testModule(root, module string, args []string) error {
	temp, err := os.MkdirTemp("", "artex-tests-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	replacements, err := collectTests(root, module, temp)
	if err != nil {
		return err
	}
	if len(replacements) == 0 {
		return fmt.Errorf("no tests found in %s", module)
	}
	data, err := json.Marshal(overlay{Replace: replacements})
	if err != nil {
		return err
	}
	overlayFile := filepath.Join(temp, "overlay.json")
	if err := os.WriteFile(overlayFile, data, 0o600); err != nil {
		return err
	}
	commandArgs := append([]string{"test", "-overlay=" + overlayFile}, args...)
	fmt.Fprintf(os.Stderr, "[tests] %s: %d package-local test files\n", module, len(replacements))
	cmd := exec.Command("go", commandArgs...)
	cmd.Dir = module
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func collectTests(root, module, temp string) (map[string]string, error) {
	replacements := make(map[string]string)
	err := filepath.WalkDir(module, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		switch entry.Name() {
		case ".git", ".codegraph", "node_modules", "vendor", ".next", "dist", "out":
			return filepath.SkipDir
		}
		if path != module {
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir // Nested modules have their own invocation.
			}
		}
		if entry.Name() != "tests" {
			return nil
		}
		if path == filepath.Join(root, "tests") {
			return filepath.SkipDir // Runner tests are executed using explicit file names.
		}
		files, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), "_test.go") {
				continue
			}
			source := filepath.Join(path, file.Name())
			virtual := filepath.Join(filepath.Dir(path), file.Name())
			if _, err := os.Stat(virtual); !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("test overlay would overwrite %s", virtual)
			}
			data, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			text := strings.ReplaceAll(string(data), "\r\n", "\n")
			if !strings.HasPrefix(text, sourceHeader) {
				return fmt.Errorf("%s must start with %q", source, sourceHeader)
			}
			rel, err := filepath.Rel(module, source)
			if err != nil {
				return err
			}
			staged := filepath.Join(temp, rel)
			if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(staged, []byte(strings.TrimPrefix(text, sourceHeader)), 0o600); err != nil {
				return err
			}
			replacements[virtual] = staged
		}
		return filepath.SkipDir
	})
	return replacements, err
}
