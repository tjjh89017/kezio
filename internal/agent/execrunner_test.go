/*
Copyright 2026.

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

package agent

import (
	"context"
	"strings"
	"testing"
)

func TestExecRunner_CapturesOutput(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), nil, "echo", "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("output = %q, want %q", out, "hello")
	}
}

func TestExecRunner_FeedsStdin(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), []byte("from-stdin"), "cat")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(out) != "from-stdin" {
		t.Errorf("output = %q, want %q", out, "from-stdin")
	}
}

func TestExecRunner_NonZeroExitIsAnError(t *testing.T) {
	r := ExecRunner{}
	if _, err := r.Run(context.Background(), nil, "false"); err == nil {
		t.Fatal("Run: want an error for a command that exits non-zero")
	}
}
