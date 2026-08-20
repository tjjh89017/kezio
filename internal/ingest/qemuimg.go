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

package ingest

import "context"

// QemuImgInfo is the subset of `qemu-img info --output=json` this
// package needs.
type QemuImgInfo struct {
	// Format is the detected image format ("raw", "qcow2", "vmdk", ...).
	Format string
	// VirtualSizeBytes is the guest-visible disk size.
	VirtualSizeBytes int64
}

// QemuImg runs qemu-img. The exec-backed production implementation lives
// in cmd/ingest; tests use a fake.
type QemuImg interface {
	// Info inspects the image at path and reports its actual format, so
	// Run can reject a source whose bytes do not match
	// Image.spec.source.format instead of trusting the claimed format.
	Info(ctx context.Context, path string) (QemuImgInfo, error)
	// ConvertToRaw converts the image at src (in srcFormat) to a raw
	// image at dst. dst must not already exist.
	ConvertToRaw(ctx context.Context, src, srcFormat, dst string) error
}
