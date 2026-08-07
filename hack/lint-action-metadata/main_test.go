package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTempAction(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "action.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLintFile_RejectsExpressionInDescription(t *testing.T) {
	// The exact defect that broke capture-vm-console: a "${{ }}" in an
	// input description, evaluated at action-load time and fatal because
	// "runner" is not a valid context there.
	path := writeTempAction(t, `
name: example
description: example
inputs:
  logfile:
    description: >-
      Put it under ${{ runner.temp }} so a later step can find it.
    required: true
runs:
  using: composite
  steps:
    - name: noop
      shell: bash
      run: echo hi
`)

	violations, err := lintFile(path)
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("want 1 violation, got %d: %+v", len(violations), violations)
	}
	if violations[0].path != "inputs.logfile.description" {
		t.Errorf("want violation at inputs.logfile.description, got %q", violations[0].path)
	}
}

func TestLintFile_RejectsExpressionInInputDefault(t *testing.T) {
	// The same defect class, found live in collect-e2e-diagnostics-bundle:
	// a "${{ }}" in an input's default value, never overridden and so
	// never previously observed to break a run.
	path := writeTempAction(t, `
name: example
description: example
inputs:
  output-dir:
    description: Directory to write into.
    default: ${{ runner.temp }}/bundle
runs:
  using: composite
  steps:
    - name: noop
      shell: bash
      run: echo hi
`)

	violations, err := lintFile(path)
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("want 1 violation, got %d: %+v", len(violations), violations)
	}
	if violations[0].path != "inputs.output-dir.default" {
		t.Errorf("want violation at inputs.output-dir.default, got %q", violations[0].path)
	}
}

func TestLintFile_AllowsExpressionInSteps(t *testing.T) {
	path := writeTempAction(t, `
name: example
description: example
inputs:
  vm-name:
    description: Name of the VM.
    required: true
outputs:
  pod-name:
    description: Resolved pod name.
    value: ${{ steps.resolve.outputs.pod-name }}
runs:
  using: composite
  steps:
    - name: Attach (${{ inputs.vm-name }})
      shell: bash
      env:
        VM_NAME: ${{ inputs.vm-name }}
      if: ${{ inputs.vm-name != '' }}
      run: echo "${VM_NAME}"
`)

	violations, err := lintFile(path)
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("want 0 violations, got %d: %+v", len(violations), violations)
	}
}

func TestLintFile_RejectsExpressionInOutputDescription(t *testing.T) {
	path := writeTempAction(t, `
name: example
description: example
outputs:
  pod-name:
    description: The ${{ inputs.vm-name }} pod.
    value: ${{ steps.resolve.outputs.pod-name }}
runs:
  using: composite
  steps:
    - name: noop
      shell: bash
      run: echo hi
`)

	violations, err := lintFile(path)
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("want 1 violation, got %d: %+v", len(violations), violations)
	}
	if violations[0].path != "outputs.pod-name.description" {
		t.Errorf("want violation at outputs.pod-name.description, got %q", violations[0].path)
	}
}
