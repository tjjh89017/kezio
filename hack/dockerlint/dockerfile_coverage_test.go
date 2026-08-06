// Package dockerlint guards against the class of failure where a
// Dockerfile's selective COPY lines drift out of sync with the Go
// import graph of the binary it builds: the container build context
// ends up missing a package that `go build ./...` on the host never
// notices, because the host build isn't restricted to the copied
// subset.
//
// For every Dockerfile that builds a kezio Go binary, this test derives
// the binary's actual transitive in-repo package set with `go list
// -deps` and confirms the Dockerfile's COPY lines cover every one of
// those packages plus the entrypoint itself.
package dockerlint

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const modulePath = "github.com/tjjh89017/kezio"

// goBuildTarget matches the `go build ... -o <name> <target>` line in a
// builder stage and captures the target package or file passed to it.
var goBuildTarget = regexp.MustCompile(`(?m)^\s*RUN.*\bgo build\b.*-o\s+\S+\s+(\S+)\s*$`)

// copyLine matches a single-source `COPY <src> <src>` line copying from
// the build context (not a multi-stage `COPY --from=...` line).
var copyLine = regexp.MustCompile(`(?m)^\s*COPY\s+(\S+)\s+\S+\s*$`)

func TestDockerfileCopyCoversGoImports(t *testing.T) {
	repoRoot := repoRootOrSkip(t)

	dockerfiles := findDockerfiles(t, repoRoot)
	if len(dockerfiles) == 0 {
		t.Fatal("no Dockerfiles found under the repo root; the discovery glob is likely broken")
	}

	for _, relPath := range dockerfiles {
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			content := readFile(t, repoRoot, relPath)

			m := goBuildTarget.FindStringSubmatch(content)
			if m == nil {
				t.Skip("no `go build` line: not a Go-binary image")
			}
			// buildArg keeps any "./" prefix, since `go list` requires
			// it for a directory target (a bare "cmd/bootd" resolves
			// against GOROOT's std packages, not the module); target is
			// the "./"-free form used to compare against COPY paths.
			buildArg := m[1]
			target := strings.TrimPrefix(buildArg, "./")

			copied := copiedPaths(content)
			if !covers(copied, target) {
				t.Fatalf("COPY lines in %s do not include the build target %q", relPath, target)
			}

			for _, dep := range transitiveKezioDeps(t, repoRoot, buildArg) {
				dir := strings.TrimPrefix(dep, modulePath+"/")
				if !covers(copied, dir) {
					t.Errorf("COPY lines in %s do not cover %q (imported by %s); "+
						"add a COPY line for it or widen an existing one", relPath, dir, target)
				}
			}
		})
	}
}

// covers reports whether some entry in copied causes path to be present
// in the build context: either an exact match (a file copy, e.g.
// "cmd/main.go") or a copied directory (ending in "/") that path falls
// under, e.g. "internal/" covers "internal/bootd".
func covers(copied []string, path string) bool {
	for _, c := range copied {
		if c == path {
			return true
		}
		if strings.HasSuffix(c, "/") && strings.HasPrefix(path+"/", c) {
			return true
		}
	}
	return false
}

func copiedPaths(content string) []string {
	matches := copyLine.FindAllStringSubmatch(content, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// transitiveKezioDeps returns the in-repo (module-local) import paths
// that target transitively depends on, via `go list -deps`.
func transitiveKezioDeps(t *testing.T, repoRoot, target string) []string {
	t.Helper()

	cmd := exec.Command("go", "list", "-deps", target)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", target, err)
	}

	var deps []string
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, modulePath+"/") {
			deps = append(deps, line)
		}
	}
	return deps
}

// findDockerfiles returns repo-root-relative paths of the root
// Dockerfile and every docker/*/Dockerfile, in the working tree (so
// uncommitted edits are checked too, not just what's already
// committed).
func findDockerfiles(t *testing.T, repoRoot string) []string {
	t.Helper()

	var files []string
	if _, err := os.Stat(filepath.Join(repoRoot, "Dockerfile")); err == nil {
		files = append(files, "Dockerfile")
	}

	matches, err := filepath.Glob(filepath.Join(repoRoot, "docker", "*", "Dockerfile"))
	if err != nil {
		t.Fatalf("glob docker/*/Dockerfile: %v", err)
	}
	for _, m := range matches {
		rel, err := filepath.Rel(repoRoot, m)
		if err != nil {
			t.Fatalf("rel %s: %v", m, err)
		}
		files = append(files, rel)
	}
	return files
}

func readFile(t *testing.T, repoRoot, relPath string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(repoRoot, relPath))
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	return string(content)
}

func repoRootOrSkip(t *testing.T) string {
	t.Helper()

	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == "/dev/null" {
		t.Skip("not inside a Go module")
	}
	return strings.TrimSuffix(gomod, "/go.mod")
}
