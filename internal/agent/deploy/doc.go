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

// Package deploy executes a DeployPlan (internal/agentapi) on the machine
// kezio-agent is running on: it writes the target disk's (and every data
// image disk's) partition table, makes swap and blank file systems, drives
// a locally spawned ezio daemon to restore every torrent slot over
// BitTorrent, runs the plan's resolved hooks in order, and hands the
// machine off to the deployed OS (systemctl reboot or poweroff, per
// DeployPlan.AfterDeploy).
//
// This package never opens or modifies a deployed disk's own file system
// content on its own initiative - the content ezio restored is
// byte-identical to the source image, so nothing needs regenerating. The
// one exception is the install-removable-fallback builtin, which mounts
// the ESP only to check for (and possibly copy in) a fallback bootloader
// file. A post hook script step runs in the live environment with no
// deployed file system mounted; a script that mounts one does so itself,
// from the device paths the plan gives it in the environment.
//
// This is the highest-blast-radius code in kezio: a wrong disk here
// destroys data with no undo. Every destructive command names a device the
// DeployPlan itself supplied, and Execute validates the whole plan before
// any command runs, so a single inconsistency aborts the deployment having
// written nothing at all.
//
// External commands and the ezio gRPC control plane are reached through
// the small Runner and EzioClient/EzioLauncher interfaces this package
// defines, not os/exec or seeder.Dial directly, so Execute's orchestration
// is unit-testable with fakes and needs no real devices or root.
package deploy
