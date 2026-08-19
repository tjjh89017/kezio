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

package bmc

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Options carries driver-independent connection settings.
type Options struct {
	// InsecureSkipVerify disables TLS cert verification for HTTPS
	// drivers. Defaults false: real BMCs commonly ship self-signed certs,
	// so opting out is a deliberate per-Machine trust decision, not a
	// blanket default.
	InsecureSkipVerify bool
}

// Driver connects to a BMC at address (already parsed, scheme included)
// using creds, and returns a BMC bound to it. Registered drivers must not
// log address or creds; a driver wrapping a lower-level client error must
// redact both first - see address.Redacted() and Credentials' doc comment.
type Driver func(ctx context.Context, address *url.URL, creds Credentials, opts Options) (BMC, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Driver{}
)

// Register is meant to be called from a driver package's init() (the
// database/sql pattern): importing internal/bmc/redfish registers
// "redfish" without this package importing it back, and other drivers
// plug in the same way without touching each other.
//
// Panics if scheme is already registered or empty - both are programming
// errors caught at package init time.
func Register(scheme string, driver Driver) {
	scheme = strings.ToLower(scheme)
	if scheme == "" {
		panic("bmc: Register called with empty scheme")
	}
	if driver == nil {
		panic("bmc: Register called with nil driver for scheme " + scheme)
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[scheme]; exists {
		panic("bmc: Register called twice for scheme " + scheme)
	}
	registry[scheme] = driver
}

// Connect parses address (e.g. "redfish://10.0.0.10/redfish/v1/Systems/1"),
// picks the driver registered for its scheme, and connects. Neither
// address nor creds is ever logged or included verbatim in an error - an
// unparsable or unknown-scheme address is reported through
// (*url.URL).Redacted(), which strips embedded userinfo but keeps host/path
// for diagnosing a misconfigured Machine.
func Connect(ctx context.Context, address string, creds Credentials, opts Options) (BMC, error) {
	parsed, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("bmc: invalid address: %w", err)
	}
	if parsed.Scheme == "" {
		return nil, fmt.Errorf("bmc: address %q has no scheme", parsed.Redacted())
	}

	scheme := strings.ToLower(parsed.Scheme)
	registryMu.RLock()
	driver, ok := registry[scheme]
	known := registeredSchemesLocked()
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("bmc: no driver registered for scheme %q (known: %s)", scheme, strings.Join(known, ", "))
	}

	bmcClient, err := driver(ctx, parsed, creds, opts)
	if err != nil {
		return nil, fmt.Errorf("bmc: connecting to %s: %w", parsed.Redacted(), err)
	}
	return bmcClient, nil
}

// registeredSchemesLocked returns the currently registered schemes,
// sorted. Callers must hold registryMu (for reading or writing).
func registeredSchemesLocked() []string {
	schemes := make([]string, 0, len(registry))
	for scheme := range registry {
		schemes = append(schemes, scheme)
	}
	sort.Strings(schemes)
	return schemes
}

// RegisteredSchemes lets callers (e.g. the Machine validating webhook)
// check registration state without going through Connect.
func RegisteredSchemes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registeredSchemesLocked()
}

// IsSchemeRegistered reports whether scheme (case-insensitive) has a
// registered Driver.
func IsSchemeRegistered(scheme string) bool {
	registryMu.RLock()
	defer registryMu.RUnlock()
	_, ok := registry[strings.ToLower(scheme)]
	return ok
}
