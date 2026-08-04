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

package v1alpha1

// FinalizerName is the finalizer set on every kezio resource that owns
// out-of-cluster or otherwise unmanaged state (image store data, seeder
// torrents, in-flight machine deployments) that a reconciler must clean up
// before the API server deletes the object.
const FinalizerName = "kezio.kojuro.date/finalizer"

// Condition types shared across kezio resources. Each kind may also define
// its own condition types for state that only applies to that kind.
const (
	// ConditionReady reports whether the resource has reached a usable,
	// steady state. Status=True once the resource is ready for use;
	// Status=False while work is in progress or after a failure.
	ConditionReady = "Ready"
)

// AnnotationBMCInsecureSkipVerify, when set to "true" on a Machine, opts
// that Machine's BMC connection out of TLS certificate verification (see
// internal/bmc.Options.InsecureSkipVerify's doc comment for why this
// exists as an explicit, per-Machine opt-out rather than a default): real
// BMCs commonly ship a self-signed certificate, and this is deliberately
// an annotation rather than a spec field - it is a transport-trust
// decision about reaching the BMC, not part of the machine's deployment
// intent, and does not warrant a CRD schema change for what is otherwise
// a narrow, environment-specific escape hatch. Any value other than
// exactly "true" (including absent) means verify.
const AnnotationBMCInsecureSkipVerify = "kezio.kojuro.date/bmc-insecure-skip-verify"
