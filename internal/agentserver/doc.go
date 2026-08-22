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

// Package agentserver implements the in-cluster HTTP endpoints
// kezio-agent talks to once it is running in the live boot environment:
//
//   - POST /agent/register: exchanges the single-use boot token minted by
//     internal/bootserver for a session credential, records the reported
//     hardware inventory as a same-name MachineHardware, and marks the
//     Machine's AgentRegistered condition True.
//   - GET/POST /agent/next: the agent's poll loop target, authenticated
//     by the session credential /agent/register minted. It answers
//     agentapi.ActionDeploy with a freshly resolved deploy plan whenever
//     the machine's current DeployRun is in a phase that expects the
//     agent to run it, and agentapi.ActionWait otherwise.
//   - POST /agent/progress: the agent's periodic step report while a
//     deploy plan runs, authenticated the same way. It persists the
//     reported step onto the DeployRun's status and answers
//     agentapi.ProgressActionAbort when that run has been deleted or is
//     being deleted, so the agent can cancel, stop its local torrents,
//     and report a final failed step.
//
// Both endpoints trust only a presented token's hash matching a live,
// unexpired value in Machine status - never the caller's identity or
// network position - the same threat model internal/bootserver's own
// doc comment describes for the boot token that gets an agent here in
// the first place.
//
// Server implements sigs.k8s.io/controller-runtime/pkg/manager.Runnable,
// so it runs embedded in the same manager process as the Machine
// reconciler: it reads and writes Machine state straight through the
// manager's client (via two field indexes - see SetupFieldIndexer)
// instead of needing its own client or a second round trip through the
// API server.
package agentserver
