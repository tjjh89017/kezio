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

package bootd

import (
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
)

// recordingSink is a minimal logr.LogSink that appends every message
// passed to Info, guarded by a mutex since the code under test logs
// from multiple goroutines (the dnsmasq supervisor's log-forwarding
// goroutines, for example). Used to assert on log content directly
// rather than on internal call counts, since lines like "served TFTP
// file" are the operator-facing signal this package's tests exist to
// pin (see tftp.go's readHandler and dnsmasq.go's Start).
type recordingSink struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recordingSink) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.msgs))
	copy(out, r.msgs)
	return out
}

func newRecordingLogger(sink *recordingSink) logr.Logger {
	return funcr.NewJSON(func(obj string) {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		sink.msgs = append(sink.msgs, obj)
	}, funcr.Options{})
}

// containsSubstring reports whether any element of msgs contains want.
func containsSubstring(msgs []string, want string) bool {
	for _, m := range msgs {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}
