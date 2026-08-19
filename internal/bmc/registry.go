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

// Package bmc holds the BMC address scheme registry. It is deliberately
// scoped to scheme lookup only: connecting to a BMC and driving power
// state is a separate, later addition that registers its schemes here
// through the same Register function.
package bmc

import (
	"sort"
	"strings"
	"sync"
)

var (
	mu      sync.RWMutex
	schemes = map[string]struct{}{}
)

// Register marks scheme (case-insensitive) as backed by a BMC driver. A
// driver package calls this from its own init() (the database/sql
// pattern), so this package never imports a concrete driver back. Every
// binary that links the Machine webhook must import the driver packages
// it wants recognized, or their schemes read as unregistered.
//
// Panics if scheme is empty or already registered: both are programming
// errors caught at package init time, before any webhook request runs.
func Register(scheme string) {
	scheme = strings.ToLower(scheme)
	if scheme == "" {
		panic("bmc: Register called with empty scheme")
	}

	mu.Lock()
	defer mu.Unlock()
	if _, exists := schemes[scheme]; exists {
		panic("bmc: Register called twice for scheme " + scheme)
	}
	schemes[scheme] = struct{}{}
}

// IsSchemeRegistered reports whether scheme (case-insensitive) has a
// registered driver.
func IsSchemeRegistered(scheme string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := schemes[strings.ToLower(scheme)]
	return ok
}

// RegisteredSchemes returns the currently registered schemes, sorted.
func RegisteredSchemes() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(schemes))
	for s := range schemes {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
