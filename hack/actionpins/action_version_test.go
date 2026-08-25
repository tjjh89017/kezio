// Package actionpins guards the class of failure where two call sites of
// the same third-party action disagree on which version to run. The
// versions drift apart one edit at a time, and the result is a workflow
// that builds an image with one major version of an action in the
// workflow and a different major version of the same action inside a
// composite action - the same build, two toolchains, and only one of
// them gets the fix when a version is bumped.
//
// The check reads the repository's own workflow and action metadata; it
// knows nothing about product code.
package actionpins

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// metadataFiles returns the repository root plus every workflow and
// composite action file below it that can hold a "uses:" step. Paths come
// back relative to the root so a failure names the file the way the
// repository does.
func metadataFiles(t *testing.T) (string, []string) {
	t.Helper()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	var files []string
	for _, pattern := range []string{
		".github/workflows/*.yaml",
		".github/workflows/*.yml",
		".github/actions/*/action.yml",
	} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, match := range matches {
			rel, err := filepath.Rel(root, match)
			if err != nil {
				t.Fatalf("relativize %s: %v", match, err)
			}
			files = append(files, rel)
		}
	}
	if len(files) == 0 {
		t.Skipf("no workflow or action metadata readable from the test's working directory")
	}
	sort.Strings(files)
	return root, files
}

// reference is one "uses: owner/repo@version" site.
type reference struct {
	file    string
	line    int
	version string
}

// externalReferences maps "owner/repo" to every site that uses it. Local
// actions ("./.github/actions/...") carry no version and are skipped.
func externalReferences(t *testing.T) map[string][]reference {
	t.Helper()

	refs := map[string][]reference{}
	root, files := metadataFiles(t)
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for i, line := range strings.Split(string(content), "\n") {
			_, after, ok := strings.Cut(line, "uses:")
			if !ok {
				continue
			}
			// A "uses:" inside a comment or a description is prose, not a
			// step; only a step's value is a bare action reference.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			value := strings.TrimSpace(after)
			if strings.HasPrefix(value, ".") || !strings.Contains(value, "@") {
				continue
			}
			action, version, _ := strings.Cut(value, "@")
			if strings.Count(action, "/") != 1 {
				continue
			}
			refs[action] = append(refs[action], reference{
				file:    file,
				line:    i + 1,
				version: version,
			})
		}
	}
	if len(refs) == 0 {
		t.Fatalf("no third-party action reference parsed; the scan is likely broken")
	}
	return refs
}

// TestThirdPartyActionsPinOneVersion proves every call site of a given
// third-party action agrees on one version.
func TestThirdPartyActionsPinOneVersion(t *testing.T) {
	for action, sites := range externalReferences(t) {
		versions := map[string][]string{}
		for _, site := range sites {
			where := site.file + ":" + strconv.Itoa(site.line)
			versions[site.version] = append(versions[site.version], where)
		}
		if len(versions) == 1 {
			continue
		}

		var report []string
		for version := range versions {
			sites := versions[version]
			sort.Strings(sites)
			report = append(report, version+" at "+strings.Join(sites, ", "))
		}
		sort.Strings(report)
		t.Errorf("%s runs at more than one version: %s; a bump that reaches only one of them "+
			"leaves the other on the old toolchain", action, strings.Join(report, "; "))
	}
}
