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

// Command kezioctl is the command-line client for kezio. It only talks
// to the Kubernetes API server and the image service's HTTP endpoint; it
// never needs qemu, partclone, or any other tool the server-side ingest
// pipeline depends on, so it can run from any operator workstation.
//
// This file only wires up cobra; every actual command implementation
// lives in internal/kezioctl so it can be unit tested directly.
package main

import "github.com/tjjh89017/kezio/internal/kezioctl"

func main() {
	kezioctl.Execute()
}
