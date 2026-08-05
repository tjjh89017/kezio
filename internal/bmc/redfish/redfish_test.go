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

package redfish

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/tjjh89017/kezio/internal/bmc"
)

// fakeRedfishServer is a minimal canned Redfish service: one service root,
// one Systems collection, and the System(s) it was built with. It records
// every reset action and boot-override PATCH it receives so tests can
// assert on the exact call a driver method issued, and lets a test mutate
// a system's PowerState to exercise GetPowerState.
type fakeRedfishServer struct {
	mu         sync.Mutex
	systemIDs  []string
	powerState map[string]string
	resetCalls []resetCall
	patchCalls []patchCall
	// wantUsername/wantPassword, when either is non-empty, makes every
	// handler require exactly this Basic Auth credential pair and
	// respond 401 otherwise - this is what TestConnectSendsBasicAuthCredentials
	// uses to confirm the driver actually presents the Credentials it was
	// given, rather than connecting unauthenticated.
	wantUsername string
	wantPassword string
}

type resetCall struct {
	systemID  string
	resetType string
}

type patchCall struct {
	systemID string
	boot     bootOverride
}

type bootOverride struct {
	Boot struct {
		BootSourceOverrideTarget  string `json:"BootSourceOverrideTarget"`
		BootSourceOverrideEnabled string `json:"BootSourceOverrideEnabled"`
	} `json:"Boot"`
}

func newFakeRedfishServer(systemIDs ...string) *fakeRedfishServer {
	f := &fakeRedfishServer{
		systemIDs:  systemIDs,
		powerState: map[string]string{},
	}
	for _, id := range systemIDs {
		f.powerState[id] = "Off"
	}
	return f
}

func (f *fakeRedfishServer) setPowerState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powerState[id] = state
}

func (f *fakeRedfishServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/redfish/v1/", func(w http.ResponseWriter, r *http.Request) {
		// This pattern ends in "/", so ServeMux treats it as a subtree
		// match: it also catches any request under /redfish/v1/ that has
		// no more specific handler (for example a bogus System path),
		// which must 404 rather than be answered with the service root.
		if r.URL.Path != "/redfish/v1/" {
			http.NotFound(w, r)
			return
		}
		// gofish fetches the service root before it authenticates (see
		// ConnectContext), so a real Redfish service typically answers
		// it anonymously; this handler does not call checkAuth for that
		// reason, matching that behavior instead of forcing every fake
		// test to pre-authenticate a request the real client never
		// sends credentials on.
		writeJSON(w, map[string]any{
			"@odata.id": "/redfish/v1/",
			"Systems":   map[string]string{"@odata.id": "/redfish/v1/Systems"},
		})
	})

	mux.HandleFunc("/redfish/v1/Systems", func(w http.ResponseWriter, r *http.Request) {
		if f.checkAuth(w, r) {
			return
		}
		members := make([]map[string]string, 0, len(f.systemIDs))
		for _, id := range f.systemIDs {
			members = append(members, map[string]string{"@odata.id": "/redfish/v1/Systems/" + id})
		}
		writeJSON(w, map[string]any{
			"@odata.id": "/redfish/v1/Systems",
			"Members":   members,
		})
	})

	for _, id := range f.systemIDs {
		mux.HandleFunc("/redfish/v1/Systems/"+id, func(w http.ResponseWriter, r *http.Request) {
			if f.checkAuth(w, r) {
				return
			}
			switch r.Method {
			case http.MethodGet:
				f.mu.Lock()
				power := f.powerState[id]
				f.mu.Unlock()
				writeJSON(w, map[string]any{
					"@odata.id":  "/redfish/v1/Systems/" + id,
					"Id":         id,
					"PowerState": power,
					"Boot":       map[string]string{"BootSourceOverrideEnabled": "Disabled", "BootSourceOverrideTarget": "None"},
					"Actions": map[string]any{
						"#ComputerSystem.Reset": map[string]string{
							"target": "/redfish/v1/Systems/" + id + "/Actions/ComputerSystem.Reset",
						},
					},
				})
			case http.MethodPatch:
				var body bootOverride
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.patchCalls = append(f.patchCalls, patchCall{systemID: id, boot: body})
				f.mu.Unlock()
				w.WriteHeader(http.StatusOK)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		})

		mux.HandleFunc("/redfish/v1/Systems/"+id+"/Actions/ComputerSystem.Reset", func(w http.ResponseWriter, r *http.Request) {
			if f.checkAuth(w, r) {
				return
			}
			var body struct {
				ResetType string `json:"ResetType"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.resetCalls = append(f.resetCalls, resetCall{systemID: id, resetType: body.ResetType})
			// A reset to On/ForceRestart/GracefulShutdown moves the fake
			// toward the matching terminal state, so a caller that reads
			// GetPowerState back after PowerOn/PowerOff observes a
			// change, the same as a real BMC eventually would.
			switch body.ResetType {
			case "On", "ForceRestart":
				f.powerState[id] = "On"
			case "GracefulShutdown", "ForceOff":
				f.powerState[id] = "Off"
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		})
	}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// checkAuth writes a 401 and returns true when wantUsername/wantPassword
// are set and the request's Basic Auth credentials do not match. It is a
// no-op (returns false) when neither is set, which is what every test
// except TestConnectSendsBasicAuthCredentials relies on.
func (f *fakeRedfishServer) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if f.wantUsername == "" && f.wantPassword == "" {
		return false
	}
	username, password, ok := r.BasicAuth()
	if !ok || username != f.wantUsername || password != f.wantPassword {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func connectTo(t *testing.T, serverURL, path string, creds bmc.Credentials) bmc.BMC {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	address := &url.URL{Scheme: "redfish+http", Host: u.Host, Path: path}

	b, err := connect(context.Background(), address, creds, bmc.Options{})
	if err != nil {
		t.Fatalf("connect() error = %v", err)
	}
	return b
}

func TestPowerOnPostsOnResetType(t *testing.T) {
	fake := newFakeRedfishServer("1")
	server := fake.start(t)

	b := connectTo(t, server.URL, "/redfish/v1/Systems/1", bmc.Credentials{Username: "admin", Password: "hunter2"})

	if err := b.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn() error = %v", err)
	}

	if len(fake.resetCalls) != 1 || fake.resetCalls[0].resetType != "On" {
		t.Fatalf("resetCalls = %+v, want one call with ResetType=On", fake.resetCalls)
	}
}

func TestPowerOffPostsGracefulShutdown(t *testing.T) {
	fake := newFakeRedfishServer("1")
	server := fake.start(t)
	fake.setPowerState("1", "On")

	b := connectTo(t, server.URL, "/redfish/v1/Systems/1", bmc.Credentials{Username: "admin", Password: "hunter2"})

	if err := b.PowerOff(context.Background()); err != nil {
		t.Fatalf("PowerOff() error = %v", err)
	}

	if len(fake.resetCalls) != 1 || fake.resetCalls[0].resetType != "GracefulShutdown" {
		t.Fatalf("resetCalls = %+v, want one call with ResetType=GracefulShutdown", fake.resetCalls)
	}
}

func TestPowerCyclePostsForceRestart(t *testing.T) {
	fake := newFakeRedfishServer("1")
	server := fake.start(t)

	b := connectTo(t, server.URL, "/redfish/v1/Systems/1", bmc.Credentials{Username: "admin", Password: "hunter2"})

	if err := b.PowerCycle(context.Background()); err != nil {
		t.Fatalf("PowerCycle() error = %v", err)
	}

	if len(fake.resetCalls) != 1 || fake.resetCalls[0].resetType != "ForceRestart" {
		t.Fatalf("resetCalls = %+v, want one call with ResetType=ForceRestart", fake.resetCalls)
	}
}

func TestGetPowerStateReadsSystemPowerState(t *testing.T) {
	tests := []struct {
		name   string
		server string
		want   bmc.PowerState
	}{
		{name: "on", server: "On", want: bmc.PowerStateOn},
		{name: "off", server: "Off", want: bmc.PowerStateOff},
		{name: "transitional maps to unknown", server: "PoweringOn", want: bmc.PowerStateUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeRedfishServer("1")
			server := fake.start(t)
			fake.setPowerState("1", tt.server)

			b := connectTo(t, server.URL, "/redfish/v1/Systems/1", bmc.Credentials{Username: "admin", Password: "hunter2"})

			got, err := b.GetPowerState(context.Background())
			if err != nil {
				t.Fatalf("GetPowerState() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("GetPowerState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetPowerStateObservesChangeAfterPowerOn(t *testing.T) {
	fake := newFakeRedfishServer("1")
	server := fake.start(t)

	b := connectTo(t, server.URL, "/redfish/v1/Systems/1", bmc.Credentials{Username: "admin", Password: "hunter2"})

	if got, err := b.GetPowerState(context.Background()); err != nil || got != bmc.PowerStateOff {
		t.Fatalf("GetPowerState() before PowerOn = %v, %v, want Off, nil", got, err)
	}

	if err := b.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn() error = %v", err)
	}

	if got, err := b.GetPowerState(context.Background()); err != nil || got != bmc.PowerStateOn {
		t.Fatalf("GetPowerState() after PowerOn = %v, %v, want On, nil", got, err)
	}
}

func TestSetOneTimePXEBootPatchesBootOverride(t *testing.T) {
	fake := newFakeRedfishServer("1")
	server := fake.start(t)

	b := connectTo(t, server.URL, "/redfish/v1/Systems/1", bmc.Credentials{Username: "admin", Password: "hunter2"})

	if err := b.SetOneTimePXEBoot(context.Background()); err != nil {
		t.Fatalf("SetOneTimePXEBoot() error = %v", err)
	}

	if len(fake.patchCalls) != 1 {
		t.Fatalf("patchCalls = %+v, want exactly one PATCH", fake.patchCalls)
	}
	got := fake.patchCalls[0].boot.Boot
	if got.BootSourceOverrideTarget != "Pxe" || got.BootSourceOverrideEnabled != "Once" {
		t.Errorf("PATCH body = %+v, want BootSourceOverrideTarget=Pxe, BootSourceOverrideEnabled=Once", got)
	}
}

// TestDeploySequenceSetPXEBootThenPowerOnThenGetPowerState walks the real
// deploy sequence end to end: SetOneTimePXEBoot, then PowerOn, then
// GetPowerState. All three operate on the same cached system, so this
// guards against an ordering regression where PowerOn's reset clobbers or
// is blocked by the boot-override PATCH that came before it - a bug that
// TestSetOneTimePXEBootPatchesBootOverride and
// TestGetPowerStateObservesChangeAfterPowerOn cannot catch on their own,
// since neither drives the sequence a deploy actually issues.
func TestDeploySequenceSetPXEBootThenPowerOnThenGetPowerState(t *testing.T) {
	fake := newFakeRedfishServer("1")
	server := fake.start(t)

	b := connectTo(t, server.URL, "/redfish/v1/Systems/1", bmc.Credentials{Username: "admin", Password: "hunter2"})

	if err := b.SetOneTimePXEBoot(context.Background()); err != nil {
		t.Fatalf("SetOneTimePXEBoot() error = %v", err)
	}
	if err := b.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn() error = %v", err)
	}
	got, err := b.GetPowerState(context.Background())
	if err != nil {
		t.Fatalf("GetPowerState() error = %v", err)
	}
	if got != bmc.PowerStateOn {
		t.Fatalf("GetPowerState() = %v, want %v", got, bmc.PowerStateOn)
	}

	if len(fake.patchCalls) != 1 {
		t.Fatalf("patchCalls = %+v, want exactly one PATCH from SetOneTimePXEBoot", fake.patchCalls)
	}
	patch := fake.patchCalls[0].boot.Boot
	if patch.BootSourceOverrideTarget != "Pxe" || patch.BootSourceOverrideEnabled != "Once" {
		t.Errorf("PATCH body = %+v, want BootSourceOverrideTarget=Pxe, BootSourceOverrideEnabled=Once", patch)
	}
	if len(fake.resetCalls) != 1 || fake.resetCalls[0].resetType != "On" {
		t.Fatalf("resetCalls = %+v, want one call with ResetType=On", fake.resetCalls)
	}
}

func TestConnectWithoutSystemPathRequiresExactlyOneSystem(t *testing.T) {
	fake := newFakeRedfishServer("1", "2")
	server := fake.start(t)

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	address := &url.URL{Scheme: "redfish+http", Host: u.Host}

	_, err = connect(context.Background(), address, bmc.Credentials{Username: "admin", Password: "hunter2"}, bmc.Options{})
	if err == nil {
		t.Fatal("connect() with an ambiguous multi-system BMC and no path succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "found 2 systems") {
		t.Errorf("connect() error = %q, want it to explain the ambiguity", err.Error())
	}
}

func TestConnectWithoutSystemPathResolvesSoleSystem(t *testing.T) {
	fake := newFakeRedfishServer("only")
	server := fake.start(t)

	b := connectTo(t, server.URL, "", bmc.Credentials{Username: "admin", Password: "hunter2"})

	if err := b.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn() error = %v", err)
	}
	if len(fake.resetCalls) != 1 || fake.resetCalls[0].systemID != "only" {
		t.Fatalf("resetCalls = %+v, want the sole system to receive the reset", fake.resetCalls)
	}
}

// TestErrorsDoNotContainCredentials drives every code path in this file
// that can return an error while holding a live Credentials value, and
// checks none of the resulting error strings contain the password. This
// guards against a future change accidentally formatting creds into an
// error (for example via a naive "%+v" on the ClientConfig or the
// Credentials struct).
func TestErrorsDoNotContainCredentials(t *testing.T) {
	const password = "correct-horse-battery-staple"
	creds := bmc.Credentials{Username: "admin", Password: password}

	fake := newFakeRedfishServer("1", "2")
	server := fake.start(t)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}

	cases := []struct {
		name    string
		address *url.URL
	}{
		{name: "ambiguous systems", address: &url.URL{Scheme: "redfish+http", Host: u.Host}},
		{name: "unresolvable system path", address: &url.URL{Scheme: "redfish+http", Host: u.Host, Path: "/redfish/v1/Systems/does-not-exist"}},
		{name: "no host", address: &url.URL{Scheme: "redfish+http"}},
		{name: "unreachable host", address: &url.URL{Scheme: "redfish+http", Host: "127.0.0.1:1"}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := connect(context.Background(), tt.address, creds, bmc.Options{})
			if err == nil {
				t.Fatal("connect() succeeded, want an error to check for leaked credentials")
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("connect() error leaked the password: %q", err.Error())
			}
		})
	}
}

// TestPowerOffRespectsUnsupportedResetType is a light sanity check that a
// BMC-declared restriction on allowed reset types is honored: the driver
// must not silently coerce an unsupported action into something the BMC
// did not agree to.
func TestPowerOffRespectsUnsupportedResetType(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/redfish/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"Systems": map[string]string{"@odata.id": "/redfish/v1/Systems"}})
	})
	mux.HandleFunc("/redfish/v1/Systems", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"Members": []map[string]string{{"@odata.id": "/redfish/v1/Systems/1"}}})
	})
	mux.HandleFunc("/redfish/v1/Systems/1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"@odata.id":  "/redfish/v1/Systems/1",
			"Id":         "1",
			"PowerState": "On",
			"Actions": map[string]any{
				"#ComputerSystem.Reset": map[string]any{
					"target":                            "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
					"ResetType@Redfish.AllowableValues": []string{"On", "ForceOff"},
				},
			},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	b := connectTo(t, server.URL, "/redfish/v1/Systems/1", bmc.Credentials{Username: "admin", Password: "hunter2"})

	err := b.PowerOff(context.Background())
	if err == nil {
		t.Fatal("PowerOff() succeeded against a system that only allows On/ForceOff, want an error")
	}
}

func TestEndpointForSchemeSelectsHTTPOnlyForRedfishPlusHTTP(t *testing.T) {
	tests := []struct {
		scheme string
		want   string
	}{
		{scheme: "redfish", want: "https://bmc.example:443"},
		{scheme: "redfishs", want: "https://bmc.example:443"},
		{scheme: "redfish+http", want: "http://bmc.example:443"},
	}

	for _, tt := range tests {
		t.Run(tt.scheme, func(t *testing.T) {
			got, err := endpointFor(&url.URL{Scheme: tt.scheme, Host: "bmc.example:443"})
			if err != nil {
				t.Fatalf("endpointFor() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("endpointFor(%s) = %q, want %q", tt.scheme, got, tt.want)
			}
		})
	}
}

func TestEndpointForRejectsAddressWithoutHost(t *testing.T) {
	_, err := endpointFor(&url.URL{Scheme: "redfish"})
	if err == nil {
		t.Fatal("endpointFor() with no host succeeded, want an error")
	}
}

// TestConnectSendsBasicAuthCredentials confirms the driver actually
// presents the Credentials it was given on the wire (via HTTP Basic Auth),
// rather than connecting unauthenticated or dropping them - the fake
// server rejects every request whose Basic Auth does not match exactly.
func TestConnectSendsBasicAuthCredentials(t *testing.T) {
	fake := newFakeRedfishServer("1")
	fake.wantUsername, fake.wantPassword = "admin", "hunter2"
	server := fake.start(t)

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	address := &url.URL{Scheme: "redfish+http", Host: u.Host, Path: "/redfish/v1/Systems/1"}

	if _, err := connect(context.Background(), address, bmc.Credentials{Username: "admin", Password: "wrong"}, bmc.Options{}); err == nil {
		t.Fatal("connect() with the wrong password succeeded, want an error")
	}

	b, err := connect(context.Background(), address, bmc.Credentials{Username: "admin", Password: "hunter2"}, bmc.Options{})
	if err != nil {
		t.Fatalf("connect() with the correct password failed: %v", err)
	}
	if err := b.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn() error = %v", err)
	}
}
