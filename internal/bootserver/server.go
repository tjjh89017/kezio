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

package bootserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines/status,verbs=get;update;patch

// Server is the HTTP handler for the boot config service: GRUB's
// unauthenticated grub.cfg fetch, and the live boot artifacts it points
// at. See the package doc comment for the full threat model; the summary
// that shapes every method below is that the requester is never trusted
// to be who its MAC address claims, so:
//   - a lookup failure, an unknown MAC, and a known machine that simply
//     does not need to net boot right now all produce the exact same
//     "boot local disk" response (see bootLocalConfig) - nothing about
//     the response shape lets a prober distinguish those cases or
//     enumerate which MACs are enrolled;
//   - the only secret ever handed out is a fresh, single-use, expiring
//     token, and only when the target machine's own state already calls
//     for a net boot.
type Server struct {
	// Client reads Machines (through the manager's cache, via the
	// MachineBootMACIndexField index - see SetupFieldIndexer) and writes
	// the minted token hash to Machine status.
	Client client.Client
	// Config holds the server's listen address, artifact directory, and
	// externally reachable URL.
	Config Config
	// Now returns the current time; overridden in tests. Nil means
	// time.Now.
	Now func() time.Time
}

var (
	_ manager.Runnable               = (*Server)(nil)
	_ manager.LeaderElectionRunnable = (*Server)(nil)
)

// New builds a Server ready to add to a controller-runtime manager
// (mgr.Add). c is typically mgr.GetClient(); the caller must also call
// SetupFieldIndexer(ctx, mgr) before the manager starts, or every lookup
// below fails.
func New(c client.Client, cfg Config) *Server {
	return &Server{Client: c, Config: cfg.withDefaults(), Now: time.Now}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: this
// server must start regardless of leader election status. Without this,
// mgr.Add's default leaves GRUB's grub.cfg fetch unanswered for up to a
// full lease duration after a rolling update, since controller-runtime
// never releases a lease on graceful shutdown by default
// (LeaderElectionReleaseOnCancel=false) and the readiness probe is
// unrelated to leadership. A machine mid net-boot cannot wait that out,
// and with a single-replica Deployment there is no other leader to defer
// to anyway.
func (s *Server) NeedLeaderElection() bool {
	return false
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Start implements manager.Runnable: it serves Handler on Config.Addr
// until ctx is cancelled, then shuts the server down gracefully.
func (s *Server) Start(ctx context.Context) error {
	httpServer := &http.Server{
		Addr: s.Config.Addr,
		// Plain HTTP by design: at boot time firmware has no TLS trust
		// store yet. ReadHeaderTimeout bounds a slow-header client from
		// tying up a connection indefinitely.
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// grubConfigNamePrefix is the fixed prefix of the grub.cfg path segment;
// the boot MAC address follows it directly (for example
// "grub.cfg-aa:bb:cc:dd:ee:01"). net/http's ServeMux patterns cannot mix
// a literal prefix with a wildcard within one path segment, so the
// route below matches the whole last segment and this handler splits it
// itself.
const grubConfigNamePrefix = "grub.cfg-"

// Handler returns the routed http.Handler, wrapped in logRequests so
// every request this server answers leaves a record (see logRequests's
// own doc comment for why that wrap is unconditional).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boot/{name}", s.handleBootPath)
	mux.Handle("GET "+artifactsPrefix, artifactsHandler(s.Config.ArtifactsDir))
	mux.HandleFunc("GET /boot/http/{name}", s.handleEFI)
	mux.HandleFunc("GET /boot/http/grub/{name}", s.handleHTTPBootGrubSearch)
	return logRequests(mux)
}

// grubNetSearchMACPrefix is the fixed prefix of the MAC-keyed name in
// GRUB's netboot config search: "grub.cfg-01-" followed by the MAC with
// dash separators ("01" is GRUB's hardware-type prefix for Ethernet, not
// part of the address).
const grubNetSearchMACPrefix = "grub.cfg-01-"

// handleHTTPBootGrubSearch answers the config search a GRUB loaded over
// UEFI HTTP Boot performs. GRUB derives its config location from the
// directory it was itself fetched from, plus a "grub/" subdirectory: a
// binary handed out as /boot/http/grubx64.efi (Config.HTTPBootURL's
// intended shape) searches /boot/http/grub/grub.cfg-<UUID>, then
// grub.cfg-01-<mac, dash-separated>, then grub.cfg-<ip>, then plain
// grub.cfg, stopping at the first hit.
//
// Only the MAC-keyed name is answered; every other name 404s on purpose,
// and that is load-bearing twice over. The UUID variant is searched
// before the MAC one, so answering it - even with boot-local - would end
// the search ahead of the only name this server can key a Machine (and
// therefore a token decision) on. And the ip/plain fallbacks after it
// carry no identity at all, so answering them would break the invariant
// that every response is decided per-machine (see Server's doc comment).
// A 404 mid-search is what GRUB expects and continues through.
func (s *Server) handleHTTPBootGrubSearch(w http.ResponseWriter, r *http.Request) {
	dashMAC, ok := strings.CutPrefix(r.PathValue("name"), grubNetSearchMACPrefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.handleGrubConfig(w, r, strings.ReplaceAll(dashMAC, "-", ":"))
}

// handleBootPath dispatches GET /boot/<name>: today the only recognized
// shape is "grub.cfg-<mac>" (see grubConfigNamePrefix); anything else is
// a plain 404, the same as an unmatched route would otherwise be.
func (s *Server) handleBootPath(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rawMAC, ok := strings.CutPrefix(name, grubConfigNamePrefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.handleGrubConfig(w, r, rawMAC)
}

// handleGrubConfig implements GET /boot/grub.cfg-<mac>: see Server's doc
// comment for the response-shape invariant every branch below preserves.
func (s *Server) handleGrubConfig(w http.ResponseWriter, r *http.Request, rawMAC string) {
	ctx := r.Context()
	log := logf.FromContext(ctx).WithName("bootserver")

	mac, ok := NormalizeMAC(rawMAC)
	if !ok {
		log.Info("grub.cfg request with a malformed MAC; boot local disk", "mac", rawMAC, "remote", r.RemoteAddr)
		writeBootLocal(w)
		return
	}

	machine, err := s.lookupMachine(ctx, mac)
	if err != nil {
		// Fail secure: a lookup error (unreachable API server, ambiguous
		// multi-match) must never be treated as "needs the live
		// environment" - the worst case of boot-local on error is one
		// extra boot cycle on the existing disk, versus handing a token
		// to whichever machine an ambiguous lookup happened to pick.
		log.Error(err, "looking up machine by boot MAC failed; boot local disk", "mac", mac)
		writeBootLocal(w)
		return
	}
	if machine == nil {
		// Info, not Error: an unknown MAC is routine for a foreign
		// machine on this L2 segment, worth logging so an operator can
		// spot a MAC that keeps asking but was never enrolled.
		log.Info("grub.cfg request for an unknown MAC; boot local disk", "mac", mac, "remote", r.RemoteAddr)
		writeBootLocal(w)
		return
	}

	if !needsNetBoot(machine) {
		writeBootLocal(w)
		return
	}

	token, err := s.rotateToken(ctx, machine)
	if err != nil {
		log.Error(err, "minting boot token failed; boot local disk", "machine", machine.Name)
		writeBootLocal(w)
		return
	}

	config, err := renderNetBootConfig(s.Config, token)
	if err != nil {
		// Fail secure: must not leak a half-rendered config with a live
		// token.
		log.Error(err, "rendering net boot config failed; boot local disk", "machine", machine.Name)
		writeBootLocal(w)
		return
	}
	writeNetBoot(w, config)
}

// lookupMachine resolves mac (already normalized) to the Machine whose
// spec.bootMACAddress matches. Returns (nil, nil) for no match - an
// ordinary, expected outcome, not an error. More than one match is an
// error: bootMACAddress uniqueness is only a convention (not enforced by
// a webhook), and picking either match for a live-environment boot and a
// token would be a guess this package is not willing to make.
func (s *Server) lookupMachine(ctx context.Context, mac string) (*keziov1alpha1.Machine, error) {
	var list keziov1alpha1.MachineList
	if err := s.Client.List(ctx, &list, client.MatchingFields{MachineBootMACIndexField: mac}); err != nil {
		return nil, fmt.Errorf("listing machines by boot MAC: %w", err)
	}
	switch len(list.Items) {
	case 0:
		return nil, nil
	case 1:
		return &list.Items[0], nil
	default:
		return nil, fmt.Errorf("%d machines share boot MAC address %s", len(list.Items), mac)
	}
}

// Phase-failure Ready condition reasons that call for a net boot retry,
// mirrored from internal/controller/machine_controller.go's unexported
// reasonInspectFailed/reasonProvisionFailed (this package must not import
// the controller package) - these string literals are the coupling; if
// those reason strings change, needsNetBoot's Error-state branch must too.
const (
	reasonInspectFailedString   = "InspectFailed"
	reasonProvisionFailedString = "ProvisionFailed"
)

// needsNetBoot maps Machine.status.state (plus, for Error, the failed
// phase on the Ready condition) onto a boot decision:
//
//   - Inspecting: agent must boot live to report hardware inventory.
//   - Provisioning: agent is writing the target disk from the live
//     environment; the disk being written is never safe to boot from yet.
//   - Error, when the failed phase was inspection or provisioning: the
//     retry re-enters the same phase, needing the live environment again.
//     Any other Error phase (e.g. RegisterFailed) does not.
//   - Enrolling: hasn't progressed to a hardware-inspecting net boot yet.
//   - Available: idle, keeps running whatever OS is already on disk.
//   - Provisioned: deployment finished; net booting again would undo it.
func needsNetBoot(machine *keziov1alpha1.Machine) bool {
	switch machine.Status.State {
	case keziov1alpha1.MachineStateInspecting, keziov1alpha1.MachineStateProvisioning:
		return true
	case keziov1alpha1.MachineStateError:
		cond := apimeta.FindStatusCondition(machine.Status.Conditions, keziov1alpha1.ConditionReady)
		if cond == nil {
			return false
		}
		return cond.Reason == reasonInspectFailedString || cond.Reason == reasonProvisionFailedString
	default:
		return false
	}
}

// rotateToken mints a fresh boot token for machine, persists its hash and
// expiry to status (overwriting whatever token was there before - see
// MachineNetBootStatus's doc comment for why that is the intended
// behavior, not a race to avoid), and returns the token itself for
// embedding in the GRUB config's cmdline.
func (s *Server) rotateToken(ctx context.Context, machine *keziov1alpha1.Machine) (string, error) {
	token, hash, err := mintToken()
	if err != nil {
		return "", err
	}

	machine.Status.NetBoot = &keziov1alpha1.MachineNetBootStatus{
		TokenHash: hash,
		ExpiresAt: metav1.NewTime(s.now().Add(s.Config.TokenTTL)),
	}
	if err := s.Client.Status().Update(ctx, machine); err != nil {
		return "", fmt.Errorf("persisting boot token hash: %w", err)
	}
	return token, nil
}

// writeBootLocal writes the fixed "boot local disk" GRUB config.
func writeBootLocal(w http.ResponseWriter) {
	w.Header().Set("Content-Type", grubContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, bootLocalConfig)
}

// writeNetBoot writes the already-rendered live-environment GRUB config
// (see renderNetBootConfig).
func writeNetBoot(w http.ResponseWriter, config string) {
	w.Header().Set("Content-Type", grubContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, config)
}
