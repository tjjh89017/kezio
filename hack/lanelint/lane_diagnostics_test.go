// Package lanelint guards the class of failure where an e2e lane fails
// in a way its own uploaded artifact cannot explain: the lane runs
// virtual machines, one of them stops making progress, and the bundle
// holds the seeder plane and the kezio objects but nothing about the
// machine that stopped - no VirtualMachineInstance, no virt-launcher
// pod, no node state, no events - so the question "why did that machine
// stop" cannot be answered after the fact at all.
//
// Every check here corresponds to one such blind spot observed in a real
// run, and reads the repository's own workflow and action metadata
// rather than any product code.
package lanelint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowPath is the only workflow that runs e2e lanes; release.yaml
// publishes artifacts and creates no machines.
const workflowPath = ".github/workflows/main.yaml"

// targetVMAction is the action a lane calls to create a KubeVirt target
// VM. A job that calls it runs machines and needs machine-plane
// diagnostics.
const targetVMAction = "./.github/actions/create-target-vm"

type workflow struct {
	Jobs map[string]struct {
		Name  string `yaml:"name"`
		Steps []struct {
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, workflowPath)); err != nil {
		t.Skipf("%s not readable from the test's working directory: %v", workflowPath, err)
	}
	return root
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(content)
}

// actionScript returns everything an action's steps run, concatenated -
// enough to answer "does this action ever ask kubectl for X".
func actionScript(t *testing.T, root, actionDir string) string {
	t.Helper()
	return readFile(t, root, filepath.Join(".github/actions", actionDir, "action.yml"))
}

func loadWorkflow(t *testing.T, root string) workflow {
	t.Helper()
	var wf workflow
	if err := yaml.Unmarshal([]byte(readFile(t, root, workflowPath)), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPath, err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("no jobs parsed out of %s; the parser is likely broken", workflowPath)
	}
	return wf
}

// TestConsoleCaptureFlushes proves a console capture that is still
// attached when the job ends has already reached the file. `script`
// buffers by default, so the last attach - the one covering the failure
// - is the one lost, which is exactly what happened to the machine that
// timed out in the two-site lane.
func TestConsoleCaptureFlushes(t *testing.T) {
	root := repoRoot(t)
	content := actionScript(t, root, "capture-vm-console")

	if !strings.Contains(content, "script -qefc") && !strings.Contains(content, "--flush") {
		t.Error("capture-vm-console runs `script` without a flush flag: a capture still attached " +
			"when the artifact is uploaded loses its buffered output, which is precisely the window " +
			"a hung machine's console has to explain")
	}
}

// TestVMLanesCollectMachinePlane proves every lane that boots machines
// uploads the state needed to say why one of them stopped.
func TestVMLanesCollectMachinePlane(t *testing.T) {
	root := repoRoot(t)
	wf := loadWorkflow(t, root)

	// An action counts as machine-plane diagnostics when it gathers
	// state (a collect-/dump- action, not one that creates or asserts)
	// and asks the cluster for VirtualMachineInstances - the object that
	// says whether a machine was ever running at all.
	collectsMachinePlane := func(uses string) bool {
		dir, ok := strings.CutPrefix(uses, "./.github/actions/")
		if !ok {
			return false
		}
		if !strings.HasPrefix(dir, "collect-") && !strings.HasPrefix(dir, "dump-") {
			return false
		}
		return strings.Contains(actionScript(t, root, dir), "vm,vmi")
	}

	var vmJobs int
	for id, job := range wf.Jobs {
		createsVMs := false
		for _, step := range job.Steps {
			if step.Uses == targetVMAction {
				createsVMs = true
				break
			}
		}
		if !createsVMs {
			continue
		}
		vmJobs++

		covered := false
		for _, step := range job.Steps {
			if collectsMachinePlane(step.Uses) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("job %q creates target VMs but no step of it collects VirtualMachineInstance "+
				"state: a machine that never runs, or stops running, leaves nothing in the uploaded "+
				"artifact to say so", id)
		}
	}
	if vmJobs == 0 {
		t.Fatalf("no job in %s uses %s; the step scan is likely broken", workflowPath, targetVMAction)
	}
}

// TestDeployWaitDetectsRestartedRun proves the wait for a deployed
// machine reports a run that restarted from the beginning instead of
// polling one phase name until the timeout. A DeployRun is one attempt:
// entering Finalizing more than once means the attempt began again, and
// the phase alone never changes to say so.
func TestDeployWaitDetectsRestartedRun(t *testing.T) {
	root := repoRoot(t)
	content := actionScript(t, root, "deploy-machine")

	if !strings.Contains(content, "phaseTimings") {
		t.Error("deploy-machine waits on status.phase alone: a DeployRun that restarts its phase " +
			"sequence reads as a run still in progress, and the lane spends its whole timeout on it")
	}
}
