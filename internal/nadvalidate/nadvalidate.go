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

// Package nadvalidate checks a Subnet's referenced
// NetworkAttachmentDefinitions against invariants the rest of kezio
// depends on but does not generate: that BootdServerIP is really the
// static address the bootd NAD hands out, that it can never be handed
// to a seeder pod from the seeder NAD's own pool, that a Site's
// tracker.ip falls inside its seeding Subnet's CIDR and outside that
// same pool, that a seeder NAD which a Site-managed tracker shares uses
// a pool-type ipam, and that a seeder NAD's static ipam choice is
// flagged once a Site's seeder concurrency outgrows what a single fixed
// address can serve.
//
// Every check takes a NAD's spec.config JSON as a plain string, so the
// ipam parsing at its core is table-testable without a fake client or
// envtest. A caller that has a NetworkAttachmentDefinition object gets
// its config string out via ConfigFromUnstructured; turning a
// CheckResult into a metav1.Condition is left to that caller.
package nadvalidate

// Verdict classifies the outcome of a single check.
type Verdict int

const (
	// OK means the check found no problem.
	OK Verdict = iota
	// Violation means the check found a real misconfiguration.
	Violation
	// Advisory means the check found a configuration that is valid
	// today but risks becoming wrong under different conditions (for
	// example, static seeder ipam that is fine for one Image at a time
	// but not for several at once). Never treat Advisory as Violation:
	// the configuration it flags is explicitly supported.
	Advisory
	// Indeterminate means the check could not reach a conclusion -
	// typically an unparseable spec.config or an ipam type this
	// package does not recognise. Indeterminate is never reported as
	// Violation: not being able to confirm a fact is not the same as
	// disproving it.
	Indeterminate
)

// CheckResult is one check's outcome. Reason is a CamelCase,
// machine-readable token (fit for metav1.Condition.Reason) so a caller
// building a Condition does not need to re-derive one from Message.
type CheckResult struct {
	Verdict Verdict
	Reason  string
	Message string
}
