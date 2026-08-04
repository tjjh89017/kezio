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

package bmc

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

// stubBMC is a no-op BMC used to verify Connect wiring without a real
// driver.
type stubBMC struct{}

func (stubBMC) PowerOn(context.Context) error                     { return nil }
func (stubBMC) PowerOff(context.Context) error                    { return nil }
func (stubBMC) PowerCycle(context.Context) error                  { return nil }
func (stubBMC) GetPowerState(context.Context) (PowerState, error) { return PowerStateOn, nil }
func (stubBMC) SetOneTimePXEBoot(context.Context) error           { return nil }

// registerTestDriver registers driver under a scheme unique to the calling
// test (derived from t.Name()) and unregisters it on cleanup, so tests can
// run with -count>1 or in parallel without colliding on the shared
// registry or tripping Register's duplicate-registration panic.
func registerTestDriver(t *testing.T, driver Driver) string {
	t.Helper()
	scheme := "test-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	Register(scheme, driver)
	t.Cleanup(func() {
		registryMu.Lock()
		delete(registry, scheme)
		registryMu.Unlock()
	})
	return scheme
}

func TestConnectDispatchesToRegisteredDriver(t *testing.T) {
	var gotAddress *url.URL
	var gotCreds Credentials
	var gotOpts Options

	scheme := registerTestDriver(t, func(_ context.Context, address *url.URL, creds Credentials, opts Options) (BMC, error) {
		gotAddress, gotCreds, gotOpts = address, creds, opts
		return stubBMC{}, nil
	})

	creds := Credentials{Username: "admin", Password: "hunter2"}
	opts := Options{InsecureSkipVerify: true}
	b, err := Connect(context.Background(), scheme+"://10.0.0.10/redfish/v1/Systems/1", creds, opts)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if b == nil {
		t.Fatal("Connect() returned a nil BMC with no error")
	}
	if gotAddress == nil || gotAddress.Host != "10.0.0.10" || gotAddress.Path != "/redfish/v1/Systems/1" {
		t.Errorf("driver received address = %+v, want host=10.0.0.10 path=/redfish/v1/Systems/1", gotAddress)
	}
	if gotCreds != creds {
		t.Errorf("driver received creds = %+v, want %+v", gotCreds, creds)
	}
	if gotOpts != opts {
		t.Errorf("driver received opts = %+v, want %+v", gotOpts, opts)
	}
}

func TestConnectUnknownSchemeReturnsClearError(t *testing.T) {
	_, err := Connect(context.Background(), "does-not-exist://host/path", Credentials{}, Options{})
	if err == nil {
		t.Fatal("Connect() with an unregistered scheme succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("Connect() error = %q, want it to name the unknown scheme", err.Error())
	}
}

func TestConnectRejectsUnparsableAddress(t *testing.T) {
	_, err := Connect(context.Background(), "://not a url", Credentials{}, Options{})
	if err == nil {
		t.Fatal("Connect() with an unparsable address succeeded, want an error")
	}
}

func TestConnectRejectsAddressWithoutScheme(t *testing.T) {
	_, err := Connect(context.Background(), "10.0.0.10/redfish/v1/Systems/1", Credentials{}, Options{})
	if err == nil {
		t.Fatal("Connect() with a schemeless address succeeded, want an error")
	}
}

func TestConnectWrapsDriverErrorWithoutLeakingCredentials(t *testing.T) {
	const password = "hunter2"
	scheme := registerTestDriver(t, func(context.Context, *url.URL, Credentials, Options) (BMC, error) {
		return nil, errors.New("simulated dial failure")
	})

	_, err := Connect(context.Background(), scheme+"://user:"+password+"@10.0.0.10/path", Credentials{Username: "user", Password: password}, Options{})
	if err == nil {
		t.Fatal("Connect() with a failing driver succeeded, want an error")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("Connect() error leaked the password embedded in the address: %q", err.Error())
	}
}

func TestRegisterPanicsOnDuplicateScheme(t *testing.T) {
	scheme := registerTestDriver(t, func(context.Context, *url.URL, Credentials, Options) (BMC, error) {
		return stubBMC{}, nil
	})

	defer func() {
		if recover() == nil {
			t.Error("Register() with an already-registered scheme did not panic")
		}
	}()
	Register(scheme, func(context.Context, *url.URL, Credentials, Options) (BMC, error) {
		return stubBMC{}, nil
	})
}

func TestRegisteredSchemesIncludesNewlyRegisteredScheme(t *testing.T) {
	scheme := registerTestDriver(t, func(context.Context, *url.URL, Credentials, Options) (BMC, error) {
		return stubBMC{}, nil
	})

	found := false
	for _, s := range RegisteredSchemes() {
		if s == scheme {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("RegisteredSchemes() = %v, want it to include %q", RegisteredSchemes(), scheme)
	}
}

func TestIsSchemeRegistered(t *testing.T) {
	scheme := registerTestDriver(t, func(context.Context, *url.URL, Credentials, Options) (BMC, error) {
		return stubBMC{}, nil
	})

	if !IsSchemeRegistered(scheme) {
		t.Errorf("IsSchemeRegistered(%q) = false, want true", scheme)
	}
	if !IsSchemeRegistered(strings.ToUpper(scheme)) {
		t.Errorf("IsSchemeRegistered(%q) = false, want true (case-insensitive)", strings.ToUpper(scheme))
	}
	if IsSchemeRegistered("does-not-exist") {
		t.Error("IsSchemeRegistered(\"does-not-exist\") = true, want false")
	}
}

func TestRegisterPanicsOnEmptyScheme(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Register() with an empty scheme did not panic")
		}
	}()
	Register("", func(context.Context, *url.URL, Credentials, Options) (BMC, error) {
		return stubBMC{}, nil
	})
}
