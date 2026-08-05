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
// mgr.Add's default (any Runnable that does not implement
// LeaderElectionRunnable, or that implements it and returns true, is
// placed in the manager's leader-election-gated group - see
// controller-runtime's runnables.Add) leaves GRUB's grub.cfg fetch and
// the live artifact download unanswered for as long as the previous
// leader's lease has not yet expired after a rolling update - by
// default, controller-runtime never releases a lease on graceful
// shutdown (LeaderElectionReleaseOnCancel defaults false, see
// cmd/main.go), so a redeployed manager pod can pass its readiness probe
// (a static ping, unrelated to leadership) while this server stays dark
// for up to a full lease duration. A machine mid net-boot cannot wait
// out that gap, and with the Deployment always running a single replica
// (config/manager/manager.yaml has no replica count override) there is
// no "other leader" this server would ever need to defer to anyway.
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
		// This handler serves firmware and GRUB clients directly, over
		// plain HTTP by design: at boot time the firmware has no TLS
		// trust store to validate a certificate against yet.
		// ReadHeaderTimeout bounds a slow-header client
		// from tying up a connection indefinitely; it is the one
		// http.Server hardening knob that has no legitimate reason to be
		// left unset for an Internet-reachable-shaped listener like this
		// one.
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

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boot/{name}", s.handleBootPath)
	mux.Handle("GET "+artifactsPrefix, artifactsHandler(s.Config.ArtifactsDir))
	mux.HandleFunc("GET /boot/http/{name}", s.handleEFI)
	return mux
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
		// Fail secure: a lookup error (API server unreachable, an
		// ambiguous multi-match - see lookupMachine) must never be
		// treated as "this machine needs the live environment". The
		// worst outcome of handing back boot-local on an error is a
		// machine that stays on its existing disk one boot cycle
		// longer; the worst outcome of the opposite mistake is handing
		// a live-environment boot (and a token) to whichever machine an
		// ambiguous lookup happened to pick.
		log.Error(err, "looking up machine by boot MAC failed; boot local disk", "mac", mac)
		writeBootLocal(w)
		return
	}
	if machine == nil {
		// Deliberately Info, not Error: an unknown MAC is the expected,
		// routine case for every foreign machine sharing this L2
		// segment (see the package doc comment) - it is not a fault
		// condition, but it is worth a log line so an operator can spot
		// a MAC that keeps asking and was never enrolled.
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

	writeNetBoot(w, s.Config, token)
}

// lookupMachine resolves mac (already normalized) to the Machine whose
// spec.bootMACAddress matches, using the MachineBootMACIndexField index.
// It returns (nil, nil) for no match - not an error - since "unknown
// MAC" is an ordinary, expected outcome (see handleGrubConfig). More than
// one match is an error: bootMACAddress is meant to be unique per
// machine, so this is a misconfiguration the field indexer alone cannot
// prevent (it is not enforced by a webhook), and picking either match
// for a live-environment boot and a token would be a guess this package
// is not willing to make.
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

// Phase-failure Ready condition reasons that call for a net boot to
// retry, mirrored from internal/controller/machine_controller.go's
// unexported reason constants of the same string value (reasonInspectFailed,
// reasonProvisionFailed): this package does not import the controller
// package (it must not depend on reconciler internals), so the two
// string literals below are the coupling - if those reason strings ever
// change, needsNetBoot's Error-state branch must change with them.
const (
	reasonInspectFailedString   = "InspectFailed"
	reasonProvisionFailedString = "ProvisionFailed"
)

// needsNetBoot reports whether machine currently needs to load the live
// boot environment over the network, as opposed to booting its local
// disk. This is the boot flow's mapping from Machine.status.state (plus,
// for the Error state, the failed phase recorded on the Ready condition)
// onto a boot decision:
//
//   - Inspecting: the controller is waiting on the agent to boot and
//     report hardware inventory - it can only do that from the live
//     environment.
//   - Provisioning: the agent is writing the target disk from the live
//     environment; the disk being written is never a safe thing to boot
//     from yet.
//   - Error, when the failed phase was inspection or provisioning: the
//     retry re-enters the same phase (see the controller's
//     reconcileError), so it needs the live environment again for the
//     same reason as above. An Error from any other phase (for example
//     RegisterFailed, a BMC-side failure that never reached a net boot)
//     does not.
//   - Enrolling: registration with the deployer has not progressed to a
//     hardware-inspecting net boot yet.
//   - Available: the machine is idle with nothing to do; it keeps
//     running whatever OS is already on its disk.
//   - Provisioned: the deployment already finished and the machine
//     should be running the OS that was just written to disk - net
//     booting it again would undo the whole point of finishing a
//     deployment.
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

// writeNetBoot writes the live-environment GRUB config carrying token.
func writeNetBoot(w http.ResponseWriter, cfg Config, token string) {
	w.Header().Set("Content-Type", grubContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, renderNetBootConfig(cfg, token))
}
