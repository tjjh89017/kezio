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

// Package agentapi defines the wire protocol between kezio-agent (running
// in the live boot environment) and the controller's agent registration
// endpoint (internal/agentserver). It holds only request/response types
// and their JSON shape - no HTTP client, no HTTP server, no
// controller-runtime dependency - so the agent binary that imports it
// stays free of everything the manager process needs (client-go,
// controller-runtime, and their transitive weight) that a binary running
// in a minimal live environment has no use for.
package agentapi

import keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"

// RegisterPath and NextPathPrefix are the HTTP routes the agent talks to,
// relative to the kezio.server= base URL the kernel cmdline carries (see
// internal/bootserver's renderNetBootConfig). NextPathPrefix is a prefix,
// not a full path: the machine name the agent registered as follows it,
// for example "/agent/machines/node-01/next".
const (
	RegisterPath   = "/agent/register"
	NextPathPrefix = "/agent/machines/"
	NextPathSuffix = "/next"
)

// RegisterRequest is the POST /agent/register request body: the full
// hardware inventory the agent collected from the live OS. It reuses
// keziov1alpha1.MachineHardwareStatus directly as the wire shape - the
// same struct Machine.status.hardware stores - instead of a parallel DTO,
// since the two are the same data by construction and a second type would
// only be able to drift out of sync with the first.
type RegisterRequest struct {
	Hardware keziov1alpha1.MachineHardwareStatus `json:"hardware"`
}

// RegisterResponse is the POST /agent/register success response.
type RegisterResponse struct {
	// MachineName is the name of the Machine the presented token
	// resolved to. The agent uses it to build the GET .../next URL.
	MachineName string `json:"machineName"`
}

// NextResponse is the GET /agent/machines/<name>/next response. Only
// ActionWait exists today: deploy actions (partition, write, finalize)
// are a later work item, so the poll loop has nothing to act on yet -
// this endpoint exists so the agent's poll loop, and the controller side
// of it, are both already in place when that work lands.
type NextResponse struct {
	Action string `json:"action"`
}

// ActionWait is the only NextResponse.Action value today: there is
// nothing for the agent to do yet.
const ActionWait = "wait"

// ErrorResponse is the JSON body returned alongside any non-2xx
// response.
type ErrorResponse struct {
	Error string `json:"error"`
}
