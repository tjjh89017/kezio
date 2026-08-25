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

// Package kezioctl implements the kezioctl CLI's commands and the logic
// behind them, kept separate from cmd/kezioctl (which only calls
// NewRootCmd and Execute) so every command can be unit tested without
// going through cobra.
//
// A command reaches at most two backends, and the two authenticate
// independently. Everything that reads or writes kezio CustomResources
// goes through the Kubernetes API on the caller's own kubeconfig
// credentials (NewClient), always as a controller-runtime client.Client
// interface and never a generated typed clientset, so the same command
// logic runs unmodified against a fake client in tests. Uploading an
// image file instead streams it to internal/imageservice over HTTP,
// addressed and authorized by --server/--token (or KEZIOCTL_SERVER,
// KEZIOCTL_TOKEN, KEZIOCTL_TOKEN_FILE - see ResolveServerURL and
// ResolveToken). That bearer token is a shared secret provisioned out of
// band and unrelated to the kubeconfig, so a working kubectl alone is not
// enough to upload.
//
// No command inspects an image file's content locally. `image upload`
// sends the bytes and creates the ImageImport naming them; the partition
// table, and every partition's role, file system, and size, are
// discovered by the ingest Job in the cluster. The operator's machine
// therefore needs no qemu-img, sfdisk, or partclone.
//
// Command state lives in structs threaded down from NewRootCmd, never in
// package-level variables, so building the command tree more than once in
// one process (which the tests do) leaks nothing between cases.
package kezioctl
