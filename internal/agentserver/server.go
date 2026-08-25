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

package agentserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/bootserver"
	"github.com/tjjh89017/kezio/internal/planbuild"
)

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machinehardwares,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=deployruns,verbs=get;list;watch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=deployruns/status,verbs=get;update;patch

// Server is the HTTP handler kezio-agent registers with. See the package
// doc comment for the full threat model.
type Server struct {
	// Client reads and writes Machines, MachineHardwares, and DeployRuns
	// (through the manager's cache; the two token-hash lookups need
	// SetupFieldIndexer run at manager start).
	Client client.Client
	// Config holds the server's listen address, session TTL, and poll
	// interval.
	Config Config
	// Now returns the current time; overridden in tests. Nil means
	// time.Now.
	Now func() time.Time
	// Abort decides whether a running DeployRun should abort, given the
	// progress report the agent just sent. Nil means POST /agent/progress
	// always answers ActionContinue.
	Abort AbortDecider
	// PlanBuilder resolves a Machine's deploy intent into a DeployPlan.
	// GET/POST /agent/next rebuilds the plan fresh from the current
	// DeployRun on every call rather than reading one stored anywhere -
	// Build is deterministic in machine/run/cluster state, so there is
	// nothing to keep in sync by persisting its output. Nil (a server not
	// wired for provisioning, or a build in this package's own tests that
	// does not exercise it) means /agent/next never answers ActionDeploy.
	PlanBuilder *planbuild.Builder
}

// AbortDecider decides whether the DeployRun a ProgressRequest reports
// against should abort. machineName, runName, and runUID come from the
// authenticated session and the request body respectively; a decider
// wired to watch DeployRun deletion is expected to compare runUID
// against the DeployRun it still knows about, so a report from a
// already-superseded run cannot suppress an abort meant for it.
type AbortDecider func(ctx context.Context, machineName, runName, runUID string) bool

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
	mux.HandleFunc("GET "+agentapi.NextPath, s.handleNext)
	mux.HandleFunc("POST "+agentapi.NextPath, s.handleNext)
	mux.HandleFunc("POST "+agentapi.ProgressPath, s.handleProgress)
	return requireAgentSchemaVersion(mux)
}

// requireAgentSchemaVersion wraps next so every route it serves rejects a
// request whose AgentSchemaVersionHeader is missing or does not equal
// agentapi.AgentSchemaVersion, before the request ever reaches a route
// handler - an agent build speaking an incompatible wire shape must never
// get far enough to have its body decoded as this build understands it.
func requireAgentSchemaVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get(agentapi.AgentSchemaVersionHeader)
		version, err := strconv.Atoi(header)
		if header == "" || err != nil || version != agentapi.AgentSchemaVersion {
			writeJSON(w, http.StatusBadRequest, agentapi.ErrorResponse{Error: "missing or unsupported " + agentapi.AgentSchemaVersionHeader})
			return
		}
		next.ServeHTTP(w, r)
	})
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

// handleRegister implements POST /agent/register: validate the presented
// boot token, create-or-update the resolved Machine's same-name
// MachineHardware from the request body's inventory, and only then mint
// a session credential and invalidate the boot token (single-use) - see
// the package doc comment for why every rejection path below produces
// the same response, and ensureMachineHardware's doc comment for why it
// runs before the token is ever invalidated.
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

	if err := s.ensureMachineHardware(ctx, machine, req.Hardware); err != nil {
		log.Error(err, "writing MachineHardware failed; boot token left valid for retry", "machine", machine.Name)
		writeJSON(w, http.StatusInternalServerError, agentapi.ErrorResponse{Error: internalErrorMessage})
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
		current := &keziov1alpha3.Machine{}
		if err := s.Client.Get(ctx, key, current); err != nil {
			return err
		}
		ingestRegistration(current, sessionHash, s.now(), s.Config.sessionTTL())
		return s.Client.Status().Update(ctx, current)
	}); err != nil {
		log.Error(err, "persisting agent registration failed", "machine", machine.Name)
		writeJSON(w, http.StatusInternalServerError, agentapi.ErrorResponse{Error: internalErrorMessage})
		return
	}

	log.Info("agent registered", "machine", machine.Name)
	writeJSON(w, http.StatusOK, agentapi.RegisterResponse{
		MachineName:       machine.Name,
		SessionToken:      sessionToken,
		SessionTTLSeconds: int64(s.Config.sessionTTL().Seconds()),
	})
}

// ingestRegistration applies a successful registration to machine's
// status in place: the AgentRegistered condition (True), the freshly
// minted session, and the single-use boot token's invalidation. Callers
// persist the result. It never touches machine.status.hardware - that is
// MachineHardware, a separate object ensureMachineHardware writes.
func ingestRegistration(machine *keziov1alpha3.Machine, sessionHash string, now time.Time, sessionTTL time.Duration) {
	if machine.Status.NetBoot != nil {
		machine.Status.NetBoot.TokenHash = ""
	}
	machine.Status.AgentSession = newAgentSessionStatus(sessionHash, now, sessionTTL)

	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.MachineConditionAgentRegistered,
		Status:             metav1.ConditionTrue,
		Reason:             "Registered",
		Message:            "kezio-agent registered and reported its hardware inventory",
		ObservedGeneration: machine.Generation,
		LastTransitionTime: metav1.NewTime(now),
	})
}

// ensureMachineHardware create-or-updates the MachineHardware named the
// same as machine, owned by it as a controller reference, with spec set
// to hardware. It runs before handleRegister invalidates the boot token
// or mints a session, so a failure here (a transient API error) leaves
// the token valid for the agent to simply retry the whole registration,
// instead of ending up consumed with no inventory ever recorded.
func (s *Server) ensureMachineHardware(ctx context.Context, machine *keziov1alpha3.Machine, hardware keziov1alpha3.MachineHardwareSpec) error {
	key := client.ObjectKey{Name: machine.Name, Namespace: machine.Namespace}
	hw := &keziov1alpha3.MachineHardware{}
	err := s.Client.Get(ctx, key, hw)
	switch {
	case apierrors.IsNotFound(err):
		hw = &keziov1alpha3.MachineHardware{
			ObjectMeta: metav1.ObjectMeta{
				Name:            machine.Name,
				Namespace:       machine.Namespace,
				OwnerReferences: []metav1.OwnerReference{machineOwnerReference(machine)},
			},
			Spec: hardware,
		}
		return s.Client.Create(ctx, hw)
	case err != nil:
		return fmt.Errorf("getting MachineHardware %q: %w", machine.Name, err)
	default:
		hw.Spec = hardware
		hw.OwnerReferences = []metav1.OwnerReference{machineOwnerReference(machine)}
		return s.Client.Update(ctx, hw)
	}
}

// machineOwnerReference builds the controller owner reference every
// MachineHardware this package writes carries. Built by hand instead of
// through controllerutil.SetControllerReference so this package needs no
// runtime.Scheme dependency, matching internal/deployer.FakeDeployer's
// own MachineHardware owner reference.
func machineOwnerReference(machine *keziov1alpha3.Machine) metav1.OwnerReference {
	blockOwnerDeletion := true
	controller := true
	return metav1.OwnerReference{
		APIVersion:         keziov1alpha3.GroupVersion.String(),
		Kind:               "Machine",
		Name:               machine.Name,
		UID:                machine.UID,
		BlockOwnerDeletion: &blockOwnerDeletion,
		Controller:         &controller,
	}
}

// handleNext implements GET/POST /agent/next: the agent's poll loop
// target. It requires the session credential minted at registration
// (RegisterResponse.SessionToken, presented the same way as the boot
// token: "Authorization: Bearer <token>"), resolved directly to its
// Machine through MachineSessionTokenHashIndexField. It answers
// ActionDeploy with a freshly built plan when the Machine's
// status.currentRunRef names a DeployRun in a phase that expects the
// agent to run it (see deployRunExpectsAgent); ActionWait otherwise,
// including while PlanBuilder.Build itself fails - see
// resolveDeployPlan's doc comment.
func (s *Server) handleNext(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.FromContext(ctx).WithName("agentserver")

	token, ok := bearerToken(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	machine, err := s.lookupMachineBySession(ctx, token)
	if err != nil {
		log.Error(err, "looking up machine by session token failed")
		writeUnauthorized(w)
		return
	}
	if machine == nil {
		writeUnauthorized(w)
		return
	}

	resp := agentapi.NextResponse{
		Action:              agentapi.ActionWait,
		PollIntervalSeconds: int32(s.Config.pollInterval().Seconds()),
	}
	if plan := s.resolveDeployPlan(ctx, machine, log); plan != nil {
		resp.Action = agentapi.ActionDeploy
		resp.Plan = plan
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveDeployPlan builds the DeployPlan to hand machine at /agent/next,
// or nil when there is none to hand out yet: no PlanBuilder configured,
// no current run, the current run's DeployRun is missing or in a phase
// that does not expect the agent (see deployRunExpectsAgent), or building
// the plan itself failed. A build failure is logged and treated exactly
// like "nothing to hand out yet" rather than surfaced as an error
// response: internal/planbuild.Builder.Build can fail transiently (a
// momentary API error) or because the Machine/DeployRun genuinely is not
// ready, and either way the correct agent-facing answer is the same
// ActionWait a still-Pending run would get.
func (s *Server) resolveDeployPlan(ctx context.Context, machine *keziov1alpha3.Machine, log logr.Logger) *agentapi.DeployPlan {
	if s.PlanBuilder == nil || machine.Status.CurrentRunRef == nil {
		return nil
	}

	run, err := s.getCurrentRun(ctx, machine)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "getting current DeployRun failed", "machine", machine.Name, "run", machine.Status.CurrentRunRef.Name)
		}
		return nil
	}
	if !deployRunExpectsAgent(run.Status.Phase) {
		return nil
	}

	claim, err := resolveClaimIntent(ctx, s.Client, machine)
	if err != nil {
		log.Error(err, "resolving MachineClaim for /agent/next failed; answering wait", "machine", machine.Name)
		return nil
	}

	plan, _, err := s.PlanBuilder.Build(ctx, machine, claim, run)
	if err != nil {
		log.Error(err, "building deploy plan for /agent/next failed; answering wait", "machine", machine.Name, "run", run.Name)
		return nil
	}
	return plan
}

// getCurrentRun resolves machine.status.currentRunRef to its DeployRun,
// defaulting to machine's own namespace when the ref carries none - the
// same convention internal/controller.MachineReconciler.getRun uses.
func (s *Server) getCurrentRun(ctx context.Context, machine *keziov1alpha3.Machine) (*keziov1alpha3.DeployRun, error) {
	ns := machine.Status.CurrentRunRef.Namespace
	if ns == "" {
		ns = machine.Namespace
	}
	var run keziov1alpha3.DeployRun
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: machine.Status.CurrentRunRef.Name}, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// deployRunExpectsAgent reports whether phase is one where kezio-agent is
// expected to be actively running the plan: every non-terminal phase
// once the manager has validated it can build one (DeployRunPhasePending
// through DeployRunPhaseFinalizing). An empty phase (Provision has not
// validated the plan yet) and the two terminal phases both report false.
func deployRunExpectsAgent(phase string) bool {
	switch phase {
	case keziov1alpha3.DeployRunPhasePending,
		keziov1alpha3.DeployRunPhasePartitioning,
		keziov1alpha3.DeployRunPhaseWritingContent,
		keziov1alpha3.DeployRunPhaseRunningPostHook,
		keziov1alpha3.DeployRunPhaseFinalizing:
		return true
	default:
		return false
	}
}

// lookupMachineByToken resolves token (unhashed, as presented) to the
// Machine whose status.netBoot.tokenHash matches its hash and whose
// token has not expired, using MachineTokenHashIndexField. It returns
// (nil, nil) - not an error - for no match, an expired match, or more
// than one match: every one of those is treated identically by the
// caller.
func (s *Server) lookupMachineByToken(ctx context.Context, token string) (*keziov1alpha3.Machine, error) {
	hash := bootserver.HashToken(token)

	var list keziov1alpha3.MachineList
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

// lookupMachineBySession resolves token (unhashed, as presented) to the
// Machine whose status.agentSession.tokenHash matches its hash and whose
// session has not expired, using MachineSessionTokenHashIndexField. It
// returns (nil, nil) - not an error - for no match, an expired match, or
// more than one match, the same undifferentiated-401 shape
// lookupMachineByToken uses.
func (s *Server) lookupMachineBySession(ctx context.Context, token string) (*keziov1alpha3.Machine, error) {
	hash := bootserver.HashToken(token)

	var list keziov1alpha3.MachineList
	if err := s.Client.List(ctx, &list, client.MatchingFields{MachineSessionTokenHashIndexField: hash}); err != nil {
		return nil, err
	}
	if len(list.Items) != 1 {
		return nil, nil
	}

	machine := &list.Items[0]
	if machine.Status.AgentSession == nil || !machine.Status.AgentSession.ExpiresAt.After(s.now()) {
		return nil, nil
	}
	return machine, nil
}

// maxProgressBodyBytes bounds the progress request body: a single step
// report is a handful of short fields, so this is a generous ceiling
// against a misbehaving or hostile agent sending an unbounded body.
const maxProgressBodyBytes = 64 << 10 // 64 KiB

// handleProgress implements POST /agent/progress: the agent's periodic
// step report while a DeployPlan runs. It requires the same session
// credential as GET/POST /agent/next. The response is ActionContinue
// unless s.Abort is set and returns true for this report. A failure to
// persist the report onto its DeployRun is logged, not surfaced to the
// agent: a missed status write does not itself justify the agent giving
// up on an otherwise-healthy deploy, and the next report gets another
// chance to land.
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.FromContext(ctx).WithName("agentserver")

	token, ok := bearerToken(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	machine, err := s.lookupMachineBySession(ctx, token)
	if err != nil {
		log.Error(err, "looking up machine by session token failed")
		writeUnauthorized(w)
		return
	}
	if machine == nil {
		writeUnauthorized(w)
		return
	}

	var req agentapi.ProgressRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxProgressBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, agentapi.ErrorResponse{Error: "malformed request body"})
		return
	}

	action := agentapi.ProgressActionContinue
	if s.Abort != nil && s.Abort(ctx, machine.Name, req.RunName, req.RunUID) {
		action = agentapi.ProgressActionAbort
	}
	if err := s.persistProgress(ctx, machine, req); err != nil {
		log.Error(err, "persisting agent progress failed", "machine", machine.Name, "run", req.RunName)
	}
	writeJSON(w, http.StatusOK, agentapi.ProgressResponse{Action: action})
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
