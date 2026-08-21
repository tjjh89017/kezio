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

// Package agent implements kezio-agent's own logic: reading its
// registration endpoint and boot token off the kernel cmdline,
// collecting hardware inventory from the live OS, and registering with
// the controller (internal/agentserver on the other end). cmd/agent is
// the thin binary wrapper around this package.
//
// Everything here that touches the filesystem (Collect) takes an
// explicit root directory instead of hard-coding "/", so tests run
// against fixture trees under testdata/ instead of the real machine's
// /sys and /proc.
package agent
