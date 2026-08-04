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

// Package agentserver is the controller-side HTTP endpoint kezio-agent
// registers with once it boots in the live environment: POST
// /agent/register ingests the presented single-use boot token and the
// agent's hardware inventory; GET /agent/machines/<name>/next is the
// agent's poll loop target.
//
// Threat model summary (see internal/bootserver's package doc comment for
// the sibling boundary this one continues): the requester here has
// already received a token minted by internal/bootserver's grub.cfg
// response, embedded in the kernel cmdline of a kernel/initrd it fetched
// over plain HTTP. That is a weaker bearer than a TLS client certificate,
// so this package treats every presented token as a claim to verify, not
// a fact:
//
//   - The token is never compared against a plaintext secret. Only its
//     SHA-256 hash is ever persisted (Machine.status.netBoot.tokenHash,
//     minted by internal/bootserver); this package hashes the presented
//     token the same way (bootserver.HashToken) and looks up the Machine
//     by that hash.
//   - Every rejection path - missing header, malformed header, no
//     matching Machine, an expired token, more than one Machine somehow
//     sharing a hash - returns the exact same 401 response body. Nothing
//     about the response shape lets a caller distinguish "wrong token"
//     from "right token, wrong machine" from "token expired ten seconds
//     ago", the same fail-secure, non-distinguishing posture
//     internal/bootserver's grub.cfg handler already applies to unknown
//     MACs.
//   - The token is single-use: a successful registration clears
//     status.netBoot.tokenHash immediately, so replaying the same POST
//     body (the same Authorization header) a second time resolves to no
//     Machine at all and gets the same 401 as any other unknown token.
//
// GET .../next needs its own credential, since the boot token above is
// already consumed by the time registration succeeds: a successful
// registration mints a second, separate token (RegisterResponse.
// SessionToken) whose SHA-256 hash - never the plaintext, same
// discipline as the boot token - lands in
// Machine.status.agentSession.tokenHash. It is presented the same way
// ("Authorization: Bearer <token>") and rejected with the same
// undifferentiated 401 (missing header, malformed header, no such
// Machine, no live session, wrong or expired session token - all
// identical), so this endpoint cannot become a machine-name enumeration
// oracle either.
package agentserver
