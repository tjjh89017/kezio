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

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// ChecksumAlgorithm is the only checksum algorithm VerifyChecksum
// supports, matching the "<algorithm>:<hex digest>" convention used by
// Image.spec.source.checksum and internal/imageservice.
const ChecksumAlgorithm = "sha256"

// VerifyChecksum checks that the file at path matches want, a
// "<algorithm>:<hex digest>" string. An empty want is not an error: it
// means the Image was created with no checksum, so nothing is verified
// (spec.source.checksum is optional).
func VerifyChecksum(path, want string) error {
	if want == "" {
		return nil
	}

	algorithm, digest, ok := strings.Cut(want, ":")
	if !ok || algorithm == "" || digest == "" {
		return fmt.Errorf("checksum %q is not in \"<algorithm>:<hex digest>\" form", want)
	}
	if !strings.EqualFold(algorithm, ChecksumAlgorithm) {
		return fmt.Errorf("unsupported checksum algorithm %q, only %q is supported", algorithm, ChecksumAlgorithm)
	}

	f, err := os.Open(path) //nolint:gosec // path is an ingest-controlled scratch/staging path, not user input
	if err != nil {
		return fmt.Errorf("open %s for checksum verification: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("read %s for checksum verification: %w", path, err)
	}
	got := hex.EncodeToString(h.Sum(nil))

	if !strings.EqualFold(got, digest) {
		return fmt.Errorf("checksum mismatch for %s: computed sha256:%s, expected %s", path, got, want)
	}
	return nil
}
