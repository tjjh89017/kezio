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

package agentserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/bootserver"
)

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Server is the HTTP handler kezio-agent registers with. See the package
// doc comment for the full threat model.
type Server struct {
	// Client reads and writes Machines (through the manager's cache; the
	// token-hash lookup needs SetupFieldIndexer run at manager start,
	// same requirement as internal/bootserver.Server.Client).
	Client client.Client
	// Config holds the server's listen address.
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
// SetupFieldIndexer(ctx, mgr) before the manager starts.
func New(c client.Client, cfg Config) *Server {
	return &Server{Client: c, Config: cfg, Now: time.Now}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: this
// server must start regardless of leader election status - see
// bootserver.Server.NeedLeaderElection's doc comment for the full
// reasoning (a redeployed manager pod can otherwise pass its readiness
// probe well before it wins leadership from a lease its predecessor
// never released, leaving kezio-agent's registration call unanswered
// for that whole window).
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
		Addr:              s.Config.Addr,
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

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+agentapi.RegisterPath, s.handleRegister)
	mux.HandleFunc("GET "+agentapi.NextPathPrefix+"{name}"+agentapi.NextPathSuffix, s.handleNext)
	mux.HandleFunc("POST "+agentapi.NextPathPrefix+"{name}"+agentapi.ProgressPathSuffix, s.handleProgress)
	return mux
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. ok is false for a missing header, a different auth scheme, or
// an empty token - every one of those is treated identically by the
// caller (see writeUnauthorized).
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(h, prefix)
	if token == "" {
		return "", false
	}
	return token, true
}

// handleRegister implements POST /agent/register: validate the presented
// token, ingest the request body's hardware inventory into the resolved
// Machine's status, and invalidate the token (single-use). See the
// package doc comment for why every rejection path below produces the
// same response.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.FromContext(ctx).WithName("agentserver")

	token, ok := bearerToken(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	machine, err := s.lookupMachineByToken(ctx, token)
	if err != nil {
		log.Error(err, "looking up machine by registration token failed")
		writeUnauthorized(w)
		return
	}
	if machine == nil {
		log.Info("registration attempt with an unrecognized, expired, or already-used token", "remote", r.RemoteAddr)
		writeUnauthorized(w)
		return
	}

	var req agentapi.RegisterRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRegisterBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, agentapi.ErrorResponse{Error: "malformed request body"})
		return
	}

	sessionToken, sessionHash, err := mintSessionToken()
	if err != nil {
		log.Error(err, "minting agent session token failed")
		writeJSON(w, http.StatusInternalServerError, agentapi.ErrorResponse{Error: internalErrorMessage})
		return
	}

	key := client.ObjectKeyFromObject(machine)
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &keziov1alpha1.Machine{}
		if err := s.Client.Get(ctx, key, current); err != nil {
			return err
		}
		ingestRegistration(current, req.Hardware, s.now())
		current.Status.AgentSession = newAgentSessionStatus(sessionHash, s.now(), s.Config.sessionTTL())
		return s.Client.Status().Update(ctx, current)
	}); err != nil {
		log.Error(err, "persisting agent registration failed", "machine", machine.Name)
		writeJSON(w, http.StatusInternalServerError, agentapi.ErrorResponse{Error: internalErrorMessage})
		return
	}

	log.Info("agent registered", "machine", machine.Name)
	writeJSON(w, http.StatusOK, agentapi.RegisterResponse{MachineName: machine.Name, SessionToken: sessionToken})
}

// maxRegisterBodyBytes bounds the registration request body: a full
// hardware inventory (disks, NICs, memory, CPU) is a few kilobytes at
// most even for a machine with dozens of disks, so this is a generous
// ceiling against a misbehaving or hostile agent sending an unbounded
// body, not a tight fit to the expected shape.
const maxRegisterBodyBytes = 1 << 20 // 1 MiB

// internalErrorMessage is the fixed ErrorResponse.Error body for every
// 500 this package returns: none of them are informative to the caller
// (a Kubernetes API failure, a marshaling bug), so there is nothing
// request-specific worth exposing.
const internalErrorMessage = "internal error"

// ingestRegistration applies a successful registration to machine's
// status in place: the reported hardware inventory, the AgentRegistered
// condition (True), and the single-use token invalidation. Callers
// persist the result.
func ingestRegistration(machine *keziov1alpha1.Machine, hardware keziov1alpha1.MachineHardwareStatus, now time.Time) {
	hw := hardware
	machine.Status.Hardware = &hw

	if machine.Status.NetBoot != nil {
		machine.Status.NetBoot.TokenHash = ""
	}

	setAgentRegisteredCondition(machine, metav1.ConditionTrue, "Registered",
		"kezio-agent registered and reported its hardware inventory", now)
}

// setAgentRegisteredCondition sets the shared AgentRegisteredCondition on
// machine.
func setAgentRegisteredCondition(machine *keziov1alpha1.Machine, status metav1.ConditionStatus, reason, message string, now time.Time) {
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha1.MachineConditionAgentRegistered,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: machine.Generation,
		LastTransitionTime: metav1.NewTime(now),
	})
}

// handleNext implements GET /agent/machines/<name>/next: the agent's poll
// loop target. It requires the session credential minted at
// registration (RegisterResponse.SessionToken, presented the same way as
// the boot token: "Authorization: Bearer <token>") and answers with a
// DeployPlan once the named Machine is Provisioning with every target
// disk resolved and every referenced Image Ready; otherwise it answers
// ActionWait.
//
// Every rejection path - missing header, malformed header, no Machine
// named <name>, no live session, a wrong or expired session token -
// returns the exact same 401 response body (writeUnauthorized), for the
// same reason internal/bootserver's and this package's own
// POST /agent/register do: nothing about the response shape may let a
// caller distinguish "no such machine" from "wrong session" from "right
// machine, expired session", which would otherwise make this endpoint an
// oracle for enumerating enrolled machine names.
func (s *Server) handleNext(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.FromContext(ctx).WithName("agentserver")

	token, ok := bearerToken(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	name := r.PathValue("name")
	machine, err := s.lookupMachineByName(ctx, name)
	if err != nil {
		log.Error(err, "looking up machine by name failed")
		writeUnauthorized(w)
		return
	}
	if machine == nil {
		writeUnauthorized(w)
		return
	}

	if !validSession(machine, token, s.now()) {
		writeUnauthorized(w)
		return
	}

	plan, err := buildDeployPlan(ctx, s.Client, s.Config, machine)
	if err != nil {
		log.Error(err, "building deploy plan failed; answering wait", "machine", machine.Name)
	}
	if plan == nil {
		writeJSON(w, http.StatusOK, agentapi.NextResponse{Action: agentapi.ActionWait})
		return
	}
	writeJSON(w, http.StatusOK, agentapi.NextResponse{Action: agentapi.ActionDeploy, Plan: plan})
}

// maxProgressBodyBytes bounds the progress request body: a snapshot of
// every partition in the largest realistic DeployPlan is still a handful
// of kilobytes, so this is a generous ceiling against a misbehaving or
// hostile agent sending an unbounded body, matching
// maxRegisterBodyBytes's reasoning.
const maxProgressBodyBytes = 1 << 20 // 1 MiB

// handleProgress implements POST /agent/machines/<name>/progress:
// kezio-agent's periodic report of a running deploy's per-partition
// status (internal/agent/deploy). It requires the same session
// credential GET .../next does, and rejects with the identical
// undifferentiated 401 handleNext's doc comment describes, for the same
// anti-enumeration reason.
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.FromContext(ctx).WithName("agentserver")

	token, ok := bearerToken(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	name := r.PathValue("name")
	machine, err := s.lookupMachineByName(ctx, name)
	if err != nil {
		log.Error(err, "looking up machine by name failed")
		writeUnauthorized(w)
		return
	}
	if machine == nil {
		writeUnauthorized(w)
		return
	}

	if !validSession(machine, token, s.now()) {
		writeUnauthorized(w)
		return
	}

	var req agentapi.ProgressRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxProgressBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, agentapi.ErrorResponse{Error: "malformed request body"})
		return
	}

	key := client.ObjectKeyFromObject(machine)
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &keziov1alpha1.Machine{}
		if err := s.Client.Get(ctx, key, current); err != nil {
			return err
		}
		setProvisioningProgressCondition(current, req, s.now())
		return s.Client.Status().Update(ctx, current)
	}); err != nil {
		log.Error(err, "persisting deploy progress failed", "machine", machine.Name)
		writeJSON(w, http.StatusInternalServerError, agentapi.ErrorResponse{Error: internalErrorMessage})
		return
	}

	writeJSON(w, http.StatusOK, agentapi.ProgressResponse{})
}

// lookupMachineByToken resolves token (unhashed, as presented) to the
// Machine whose status.netBoot.tokenHash matches its hash and whose
// token has not expired, using MachineTokenHashIndexField. It returns
// (nil, nil) - not an error - for no match, an expired match, or more
// than one match: see the package doc comment for why every one of those
// cases is treated identically by the caller.
func (s *Server) lookupMachineByToken(ctx context.Context, token string) (*keziov1alpha1.Machine, error) {
	hash := bootserver.HashToken(token)

	var list keziov1alpha1.MachineList
	if err := s.Client.List(ctx, &list, client.MatchingFields{MachineTokenHashIndexField: hash}); err != nil {
		return nil, err
	}
	if len(list.Items) != 1 {
		return nil, nil
	}

	machine := &list.Items[0]
	if machine.Status.NetBoot == nil || !machine.Status.NetBoot.ExpiresAt.After(s.now()) {
		return nil, nil
	}
	return machine, nil
}

// lookupMachineByName resolves name - the bare Machine name a poll or
// progress report's URL path carries (see MachineNameIndexField's doc
// comment for why a namespaced Get cannot do this) - to the one Machine
// with that name. It returns (nil, nil), not an error, for both zero
// matches (no such Machine) and more than one (the name is ambiguous
// across namespaces; Server has no namespace in the request to
// disambiguate with) - handleNext and handleProgress answer the
// identical undifferentiated 401 either way, the same anti-enumeration
// shape lookupMachineByToken's own doc comment describes.
func (s *Server) lookupMachineByName(ctx context.Context, name string) (*keziov1alpha1.Machine, error) {
	var list keziov1alpha1.MachineList
	if err := s.Client.List(ctx, &list, client.MatchingFields{MachineNameIndexField: name}); err != nil {
		return nil, err
	}
	if len(list.Items) != 1 {
		return nil, nil
	}
	return &list.Items[0], nil
}

// writeUnauthorized writes the fixed 401 response every rejection path
// in this package uses. It is a fixed, static body: no per-request data
// is ever interpolated into it, so every "not a valid, live,
// still-unused token for a real machine" case is byte-identical.
func writeUnauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, agentapi.ErrorResponse{Error: "unauthorized"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
