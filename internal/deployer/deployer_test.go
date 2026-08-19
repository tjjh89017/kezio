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

package deployer

import "testing"

func TestResultZeroValueOutcomeIsInvalid(t *testing.T) {
	var r Result
	if r.Outcome == Complete {
		t.Fatal("Result{} zero value has Outcome == Complete; a forgotten Outcome assignment must not read as success")
	}
	if got := r.Outcome.String(); got != "Unknown" {
		t.Errorf("Result{}.Outcome.String() = %q, want %q", got, "Unknown")
	}
}

func TestOutcomeString(t *testing.T) {
	cases := []struct {
		outcome Outcome
		want    string
	}{
		{Complete, "Complete"},
		{Continuing, "Continuing"},
		{Busy, "Busy"},
		{Failed, "Failed"},
		{Outcome(99), "Unknown"},
	}
	for _, tc := range cases {
		if got := tc.outcome.String(); got != tc.want {
			t.Errorf("Outcome(%d).String() = %q, want %q", tc.outcome, got, tc.want)
		}
	}
}
