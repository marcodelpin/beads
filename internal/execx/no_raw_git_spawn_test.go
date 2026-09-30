package execx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/testutil/bazeltest"
)

// TestNoRawGitSpawn walks every non-test Go file under internal/ and cmd/ and
// fails on a git child spawned without this package: exec.Command("git", ...)
// or exec.CommandContext(ctx, "git", ...). Such a spawn skips HideWindow and
// CREATE_NO_WINDOW, so the Windows GUI build flashes a console window for it.
//
// The population is derived here, at test time, on purpose: every sync with
// upstream imports code that spawns git directly, and a fixed list of call
// sites cannot see a site that arrives later.
func TestNoRawGitSpawn(t *testing.T) {
	if bazeltest.IsBazel() {
		t.Skip("walks the whole internal/ and cmd/ tree, which a Bazel sandbox does not declare as data; runs under go test")
	}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string
	scanned := 0

	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if filepath.Base(path) == "execx" || filepath.Base(path) == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			scanned++
			execName := ""
			for _, imp := range file.Imports {
				if path, _ := strconv.Unquote(imp.Path.Value); path == "os/exec" {
					execName = "exec"
					if imp.Name != nil {
						execName = imp.Name.Name
					}
				}
			}
			if execName == "" || execName == "_" {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != execName {
					return true
				}
				argIdx := -1
				switch sel.Sel.Name {
				case "Command":
					argIdx = 0
				case "CommandContext":
					argIdx = 1
				}
				if argIdx < 0 || len(call.Args) <= argIdx {
					return true
				}
				if namesGit(call.Args[argIdx], src, fset) {
					offenders = append(offenders, fset.Position(call.Pos()).String())
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// Positive control: a walk that found no files proves nothing.
	if scanned < 100 {
		t.Fatalf("scanned only %d Go files under internal/ and cmd/; the walk did not reach the tree", scanned)
	}
	if len(offenders) > 0 {
		t.Errorf("%d git spawn(s) bypass execx (use execx.GitCommand / execx.GitCommandContext):\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// namesGit reports whether the executable argument of an exec.Command call
// names git. A string literal must be exactly "git" or "git.exe". Any other
// expression (git.executable, gitBin, pinnedGitPath) counts when its source
// text contains "git", case-insensitive: that catches a git binary resolved
// once with exec.LookPath and pinned in a variable. It cannot see a variable
// whose name does not mention git (exe := lookGit(); exec.Command(exe, ...));
// name such variables after what they hold.
func namesGit(arg ast.Expr, src []byte, fset *token.FileSet) bool {
	if lit, ok := arg.(*ast.BasicLit); ok {
		if lit.Kind != token.STRING {
			return false
		}
		name, _ := strconv.Unquote(lit.Value)
		return name == "git" || name == "git.exe"
	}
	start, end := fset.Position(arg.Pos()).Offset, fset.Position(arg.End()).Offset
	if start < 0 || end > len(src) || start >= end {
		return false
	}
	return strings.Contains(strings.ToLower(string(src[start:end])), "git")
}
