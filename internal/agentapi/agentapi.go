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

// Package agentapi defines the wire protocol between kezio-agent (running
// in the live boot environment) and the controller's agent registration
// endpoint (internal/agentserver). It holds only request/response types
// and their JSON shape - no HTTP client, no HTTP server, no
// controller-runtime dependency - so the agent binary that imports it
// stays free of everything the manager process needs (client-go,
// controller-runtime, and their transitive weight) that a binary running
// in a minimal live environment has no use for. The one exception is
// api/v1alpha2 itself: RegisterRequest reuses MachineHardwareSpec
// directly rather than a parallel DTO, since the two are the same data
// by construction and a second type would only be able to drift out of
// sync with the first; that package imports nothing beyond
// k8s.io/apimachinery's metav1, so it carries none of client-go's or
// controller-runtime's weight.
package agentapi

import (
	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// RegisterPath and NextPath are the HTTP routes the agent talks to,
// relative to the kezio.server= base URL the kernel cmdline carries (see
// internal/bootserver's renderNetBootConfig). Both live under the
// "/agent/" prefix internal/bootd's proxy forwards to the in-cluster
// agent server - see that package's init check, which panics at process
// start if either constant here ever stops satisfying that.
const (
	RegisterPath = "/agent/register"
	NextPath     = "/agent/next"
)

// AgentSchemaVersion is the current wire schema version for this
// package's types. AgentSchemaVersionHeader carries it on every request
// the agent sends, so a future breaking change to the wire shape has
// somewhere to signal from without guessing at an agent build's
// capabilities from its behavior alone.
const AgentSchemaVersion = 1

// AgentSchemaVersionHeader is the HTTP header kezio-agent sets on every
// request it sends, carrying the AgentSchemaVersion value it was built
// against, as a decimal integer.
const AgentSchemaVersionHeader = "X-Kezio-Agent-Schema-Version"

// RegisterRequest is the POST /agent/register request body: the boot
// token authenticates the caller as the "Authorization: Bearer <token>"
// header (not a body field - see internal/bootserver's token, which the
// kernel cmdline embeds the same way), and Hardware is the full hardware
// inventory the agent collected from the live OS.
type RegisterRequest struct {
	// Hardware is the reported inventory, stored verbatim as the
	// same-name MachineHardware's spec.
	Hardware keziov1alpha2.MachineHardwareSpec `json:"hardware"`
}

// RegisterResponse is the POST /agent/register success response.
type RegisterResponse struct {
	// MachineName is the name of the Machine the presented token
	// resolved to.
	MachineName string `json:"machineName"`
	// SessionToken is a fresh credential the agent presents (as a Bearer
	// token, the same as the boot token at /agent/register) on every
	// subsequent GET/POST /agent/next call. It is distinct from the
	// single-use boot token that authenticated this request - that token
	// is already consumed by the time this response is written - so
	// polling needs its own credential. Only its SHA-256 hash is ever
	// persisted server-side (Machine.status.agentSession.tokenHash); this
	// value is returned exactly once and never recoverable again.
	SessionToken string `json:"sessionToken"`
	// SessionTTLSeconds bounds how long SessionToken is accepted on
	// GET/POST /agent/next, so the agent knows to re-register (it cannot
	// - the boot token that got it here is already consumed) well before
	// a long-running live session's session token would otherwise expire
	// out from under it.
	SessionTTLSeconds int64 `json:"sessionTTLSeconds"`
}

// NextResponse is the GET/POST /agent/next response body.
type NextResponse struct {
	// Action is always ActionWait in this build: nothing in this package
	// hands out a deploy plan yet.
	Action string `json:"action"`
	// PollIntervalSeconds tells the agent how long to wait before its
	// next /agent/next call.
	PollIntervalSeconds int32 `json:"pollIntervalSeconds"`
}

// ActionWait is the NextResponse.Action value: there is nothing for the
// agent to do yet.
const ActionWait = "wait"

// ErrorResponse is the JSON body returned alongside any non-2xx
// response.
type ErrorResponse struct {
	Error string `json:"error"`
}
