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

package bootserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=subnets,verbs=get

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
	mux.HandleFunc("GET /grub/{name}", s.handleHTTPBootGrubSearch)
	return logRequests(mux)
}

// grubNetSearchMACPrefix is the fixed prefix of the MAC-keyed name in
// GRUB's netboot config search: "grub.cfg-01-" followed by the MAC with
// dash separators ("01" is GRUB's hardware-type prefix for Ethernet, not
// part of the address).
const grubNetSearchMACPrefix = "grub.cfg-01-"

// handleHTTPBootGrubSearch answers the config search a GRUB loaded over
// UEFI HTTP Boot performs: grub.cfg-<UUID>, then grub.cfg-01-<mac,
// dash-separated>, then grub.cfg-<ip>, then plain grub.cfg, stopping at
// the first hit.
//
// It is mounted twice because where that search lands depends on how the
// GRUB binary was built. The GRUB manual has it relative to the directory
// the binary was fetched from plus "grub/" - /boot/http/grub/ for a
// binary handed out as /boot/http/grubx64.efi (bootd's HTTPBootURL's
// intended shape). Debian's signed netboot build instead bakes in the
// prefix "/grub", applied against the server root regardless of where
// the binary came from.
//
// Two names are answered. The MAC-keyed one resolves a Machine and
// decides per-machine, exactly like the colon-form route. Plain
// "grub.cfg" gets grubSearchRedirectConfig - the same fixed string for
// every requester - because Debian's HTTP-loaded GRUB skips the identity
// search and asks for that name alone; answering it is also safe for a
// GRUB that does search, since plain grub.cfg is that search's last
// resort, after every identity-keyed name. The UUID and ip names still
// 404 on purpose: UUID is searched before the MAC name, so any answer
// there would end a proper search ahead of the only name a Machine can
// be keyed on, and a 404 mid-search is what GRUB expects and continues
// through.
func (s *Server) handleHTTPBootGrubSearch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "grub.cfg" {
		w.Header().Set("Content-Type", grubContentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, grubSearchRedirectConfig)
		return
	}
	dashMAC, ok := strings.CutPrefix(name, grubNetSearchMACPrefix)
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

	renderCfg := s.Config
	subnet, err := s.resolveSubnet(ctx, machine)
	if err != nil {
		// Not fail-secure territory: the Subnet only selects which base
		// URL to embed, never whether to net boot at all. Falling back to
		// the manager-wide Config.ServerURL/AgentServerURL keeps a
		// dangling or absent SubnetRef working exactly as before this
		// override existed.
		log.Info("resolving machine's Subnet failed; using the manager-wide boot server URL",
			"machine", machine.Name, "subnetRef", machine.Spec.SubnetRef, "error", err.Error())
	} else if base, ok := subnetBootBaseURL(subnet); ok {
		renderCfg.ServerURL = base
		renderCfg.AgentServerURL = base
	}

	config, err := renderNetBootConfig(renderCfg, token, resolveConsole(machine.Spec.Console, renderCfg.DefaultConsole))
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
func (s *Server) lookupMachine(ctx context.Context, mac string) (*keziov1alpha3.Machine, error) {
	var list keziov1alpha3.MachineList
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

// resolveSubnet resolves machine's SubnetRef to the Subnet object,
// defaulting to machine's own namespace the same way every other NameRef
// in this API resolves an empty Namespace.
func (s *Server) resolveSubnet(ctx context.Context, machine *keziov1alpha3.Machine) (*keziov1alpha3.Subnet, error) {
	ns := machine.Spec.SubnetRef.Namespace
	if ns == "" {
		ns = machine.Namespace
	}
	var subnet keziov1alpha3.Subnet
	if err := s.Client.Get(ctx, client.ObjectKey{Name: machine.Spec.SubnetRef.Name, Namespace: ns}, &subnet); err != nil {
		return nil, fmt.Errorf("getting Subnet %s/%s: %w", ns, machine.Spec.SubnetRef.Name, err)
	}
	return &subnet, nil
}

// needsNetBoot maps Machine.status.state onto a boot decision:
//
//   - Inspecting: agent must boot live to report hardware inventory.
//   - Provisioning: agent is writing the target disk from the live
//     environment; the disk being written is never safe to boot from yet.
//   - Every other state (Enrolling, Available, Provisioned,
//     Deprovisioning, PoweringOff): no net boot needed.
//
// v1alpha3 has no Error top-level state - a phase failure is reported on
// the orthogonal OperationalStatus axis while State stays at whatever
// phase failed (see MachineStatus's doc comment), so a machine retrying
// a failed inspection or provisioning is still State Inspecting/
// Provisioning and this check needs no separate error-state branch to
// cover the retry.
func needsNetBoot(machine *keziov1alpha3.Machine) bool {
	switch machine.Status.State {
	case keziov1alpha3.MachineStateInspecting, keziov1alpha3.MachineStateProvisioning:
		return true
	default:
		return false
	}
}

// rotateToken mints a fresh boot token for machine, persists its hash and
// expiry to status (overwriting whatever token was there before - see
// MachineNetBootStatus's doc comment for why that is the intended
// behavior, not a race to avoid), and returns the token itself for
// embedding in the GRUB config's cmdline.
func (s *Server) rotateToken(ctx context.Context, machine *keziov1alpha3.Machine) (string, error) {
	token, hash, err := mintToken()
	if err != nil {
		return "", err
	}

	machine.Status.NetBoot = &keziov1alpha3.MachineNetBootStatus{
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
