/*
Copyright 2026 Date Huang.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// bootArtifactsDockerfile is the image fetch-boot-artifacts's cp runs
// against; its USER line is what makes the tftp emptyDir's contents
// group-readable/writable to a matching FSGroup.
const bootArtifactsDockerfile = "docker/boot-artifacts/Dockerfile"

// dockerfileUser matches a `USER <uid>:<gid>` line.
var dockerfileUser = regexp.MustCompile(`(?m)^\s*USER\s+(\d+):(\d+)\s*$`)

// TestBuildBootdDeploymentFSGroupMatchesBootArtifactsImage checks the
// pod's FSGroup against the Dockerfile's actual USER gid, not a second
// hardcoded literal, so an edited Dockerfile without a matching
// controller change fails here instead of causing EACCES at runtime - a
// failure envtest cannot observe, since it never runs a real kubelet.
func TestBuildBootdDeploymentFSGroupMatchesBootArtifactsImage(t *testing.T) {
	repoRoot := repoRootOrSkip(t)

	content, err := os.ReadFile(filepath.Join(repoRoot, bootArtifactsDockerfile))
	if err != nil {
		t.Fatalf("read %s: %v", bootArtifactsDockerfile, err)
	}

	m := dockerfileUser.FindStringSubmatch(string(content))
	if m == nil {
		t.Fatalf("%s has no `USER <uid>:<gid>` line", bootArtifactsDockerfile)
	}
	dockerfileGID, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		t.Fatalf("parse %s USER gid %q: %v", bootArtifactsDockerfile, m[2], err)
	}

	subnet := testSubnet("site-hq")
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}
	sc := buildBootdDeployment(subnet, cfg).Spec.Template.Spec.SecurityContext
	if sc == nil || sc.FSGroup == nil {
		t.Fatal("pod SecurityContext.FSGroup is nil, want it set to match the Dockerfile's USER gid")
	}

	if *sc.FSGroup != dockerfileGID {
		t.Errorf("pod FSGroup = %d, want %d to match %s's USER gid", *sc.FSGroup, dockerfileGID, bootArtifactsDockerfile)
	}
}

// repoRootOrSkip returns the repository root, or skips the test when it
// cannot be determined (for example, `go` is unavailable on PATH).
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
