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

package kezioctl

import (
	"path/filepath"
	"strings"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// DetectFormatFromFilename guesses an Image source format from a local
// file's extension, for `kezioctl image upload` when --format is not
// given. It recognizes the extensions that map unambiguously onto
// ImageSpec.Source.Format's enum (raw/qcow2/vmdk); anything else (including
// stdin, which has no filename) returns ok=false so the caller can require
// an explicit --format instead of guessing wrong.
func DetectFormatFromFilename(name string) (format string, ok bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".qcow2":
		return keziov1alpha1.ImageFormatQCOW2, true
	case ".vmdk":
		return keziov1alpha1.ImageFormatVMDK, true
	case ".raw", ".img":
		return keziov1alpha1.ImageFormatRaw, true
	default:
		return "", false
	}
}
