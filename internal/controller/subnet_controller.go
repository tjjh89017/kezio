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

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/nadvalidate"
	"github.com/tjjh89017/kezio/internal/sitederive"
	"github.com/tjjh89017/kezio/internal/subnetvalidate"
)

// bootdNamespacePrerequisiteRequeueAfter bounds how long a Subnet whose
// namespace fails checkBootdNamespacePrerequisites waits before its next
// reconcile. See that function's doc comment for why this polls instead
// of watching.
const bootdNamespacePrerequisiteRequeueAfter = time.Minute

// bootdDeploymentOwnershipRequeueAfter bounds how long a Subnet with an
// unowned bootd Deployment name collision waits before its next
// reconcile: the foreign Deployment has no owner reference back to the
// Subnet, so Owns' watch never sees its deletion.
const bootdDeploymentOwnershipRequeueAfter = time.Minute

// networkAttachmentDefinitionGVK identifies a
// k8s.cni.cncf.io/v1.NetworkAttachmentDefinition, read as
// unstructured.Unstructured since this repo has no generated client for it.
var networkAttachmentDefinitionGVK = schema.GroupVersionKind{
	Group:   "k8s.cni.cncf.io",
	Version: "v1",
	Kind:    "NetworkAttachmentDefinition",
}

// SubnetReconciler reconciles a Subnet object.
type SubnetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// BootdDeployment configures the per-Subnet bootd Deployment this
	// reconciler creates and keeps up to date. Its zero value disables
	// Deployment reconciliation entirely (see BootdDeploymentConfig);
	// validation conditions are still computed and written regardless.
	BootdDeployment BootdDeploymentConfig
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=subnets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=subnets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=subnets/finalizers,verbs=update
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=sites,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch

// Reconcile fetches the Subnet, dispatches to onDelete if it is being
// deleted, and otherwise dispatches to onChange.
//
// Subnet carries no finalizer: its bootd Deployment always lives in the
// Subnet's own namespace and is owned by it (reconcileBootdDeployment),
// so garbage collection removes it once the Subnet is gone. A finalizer
// would only earn its place if teardown needed something GC cannot do,
// and would otherwise risk stranding the Subnet in Terminating if this
// controller is uninstalled or crash-loops.
func (r *SubnetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	subnet := &keziov1alpha3.Subnet{}
	if err := r.Get(ctx, req.NamespacedName, subnet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !subnet.DeletionTimestamp.IsZero() {
		return r.onDelete(ctx, subnet)
	}

	return r.onChange(ctx, subnet)
}

// onDelete is a no-op: garbage collection removes the bootd Deployment
// via its owner reference once the API server accepts the delete.
func (r *SubnetReconciler) onDelete(ctx context.Context, _ *keziov1alpha3.Subnet) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)
	return ctrl.Result{}, nil
}

// subnetCheck pairs one internal/nadvalidate or internal/subnetvalidate
// CheckResult with whether a Violation verdict withholds the bootd
// Deployment outright (blocks) - see updateSubnetConditions.
type subnetCheck struct {
	result nadvalidate.CheckResult
	blocks bool
}

// onChange runs every validation check against subnet, ensures its bootd
// Deployment exists and matches its current spec when r.BootdDeployment
// is configured, and folds both into subnet's Valid/Ready conditions.
//
// A blocking check's Violation (BootdNetworkCollision, DHCPLeaseRangeValid,
// BootdServerIPInCIDR, or an unowned name collision) withholds the
// Deployment entirely; every other Violation (for example a
// CheckBootdAddress mismatch, or checkSiteRef's SiteNotFound) still fails
// Valid/Ready but leaves an already-serving Deployment in place - for
// SiteNotFound specifically, because bootd keeps PXE-ing machines on this
// segment regardless of whether the Site it claims to belong to still
// exists, and Ready already carries that same "misconfigured but still
// serving" meaning for every other non-blocking Violation.
func (r *SubnetReconciler) onChange(ctx context.Context, subnet *keziov1alpha3.Subnet) (ctrl.Result, error) {
	checks, err := r.runNADChecks(ctx, subnet)
	if err != nil {
		return ctrl.Result{}, err
	}

	siteCheck, err := r.checkSiteRef(ctx, subnet)
	if err != nil {
		return ctrl.Result{}, err
	}
	checks = append(checks, siteCheck)

	// A Subnet with no boot half carries no bootd Deployment at all: none
	// of the bootd-specific checks below apply, and hasBootPlane is the
	// only branch updateSubnetConditions needs to compute Ready for it.
	if !subnet.Spec.HasBootPlane() {
		if err := r.updateSubnetConditions(ctx, subnet, checks, false, false, false, "", ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	bootdServerIPResult := subnetvalidate.CheckBootdServerIPInCIDR(subnet.Spec.CIDR, subnet.Spec.BootdServerIP)
	bootdServerIPViolation := bootdServerIPResult.Verdict == nadvalidate.Violation
	checks = append(checks, subnetCheck{result: bootdServerIPResult, blocks: true})

	var leaseRangeViolation bool
	if subnet.Spec.DHCP.Mode == keziov1alpha3.SubnetDHCPModeLease {
		leaseRangeResult := subnetvalidate.CheckDHCPLeaseRange(subnet.Spec.CIDR, subnet.Spec.DHCP.LeaseRangeStart, subnet.Spec.DHCP.LeaseRangeEnd)
		leaseRangeViolation = leaseRangeResult.Verdict == nadvalidate.Violation
		checks = append(checks, subnetCheck{result: leaseRangeResult, blocks: true})
	}

	collidingSubnet, err := r.findBootdNetworkCollision(ctx, subnet)
	if err != nil {
		return ctrl.Result{}, err
	}
	if collidingSubnet != nil {
		checks = append(checks, subnetCheck{blocks: true, result: nadvalidate.CheckResult{
			Verdict: nadvalidate.Violation,
			Reason:  "BootdNetworkCollision",
			// Named as resolved, not as declared, since BootdNetworkRef.Namespace
			// can cross namespaces (findBootdNetworkCollision).
			Message: fmt.Sprintf("Subnet %s/%s also targets bootdNetworkRef %s/%s - two Subnets on the same broadcast domain would each run their own bootd, both answering every DHCPDISCOVER with no way for firmware to prefer one", collidingSubnet.Namespace, collidingSubnet.Name, resolveNamespace(*subnet.Spec.BootdNetworkRef, subnet.Namespace), subnet.Spec.BootdNetworkRef.Name),
		}})
	}

	var depConfigured, depAvailable bool
	var depReason, depMessage string
	var requeueAfter time.Duration
	switch {
	case collidingSubnet != nil:
		// The BootdNetworkCollision check above already fails Valid/Ready;
		// withhold the Deployment so this Subnet doesn't become the second
		// live responder itself.
	case leaseRangeViolation:
		// dnsmasq is the segment's sole DHCP authority in lease mode and
		// cannot serve a broken range; withhold the Deployment.
	case bootdServerIPViolation:
		// An out-of-cidr bootdServerIP is a silent mid-boot PXE timeout for
		// the whole segment; withhold the Deployment.
	case r.BootdDeployment.enabled():
		depConfigured = true
		prereqChecks, err := r.checkBootdNamespacePrerequisites(ctx, subnet)
		if err != nil {
			return ctrl.Result{}, err
		}
		checks = append(checks, prereqChecks...)
		for _, c := range prereqChecks {
			if c.result.Verdict == nadvalidate.Violation {
				// Neither prerequisite is watched (see
				// checkBootdNamespacePrerequisites), so poll instead of
				// waiting on the manager's default resync.
				requeueAfter = bootdNamespacePrerequisiteRequeueAfter
				break
			}
		}

		dep, unowned, err := r.reconcileBootdDeployment(ctx, subnet)
		if err != nil {
			return ctrl.Result{}, err
		}
		if unowned {
			checks = append(checks, subnetCheck{blocks: true, result: nadvalidate.CheckResult{
				Verdict: nadvalidate.Violation,
				Reason:  "BootdDeploymentOwnership",
				Message: fmt.Sprintf("a Deployment named %s/%s already exists and is not controlled by this Subnet - refusing to adopt or overwrite it", dep.Namespace, dep.Name),
			}})
			// The foreign Deployment has no owner reference back to subnet,
			// so Owns' watch never sees its deletion; poll instead.
			requeueAfter = bootdDeploymentOwnershipRequeueAfter
		} else {
			depAvailable = deploymentAvailable(dep)
			depReason, depMessage = deploymentUnavailableReason(dep)
		}
	default:
		depReason, depMessage = "BootdDeploymentImageUnconfigured", "no bootd Deployment image is configured on the manager (BOOTD_DEPLOYMENT_IMAGE); bootd is not reconciled for this Subnet"
	}

	if err := r.updateSubnetConditions(ctx, subnet, checks, true, depConfigured, depAvailable, depReason, depMessage); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// reconcileBootdDeployment creates subnet's bootd Deployment if absent,
// updates it in place when the desired Spec/Labels differ, and otherwise
// leaves it untouched. Deleting the Subnet removes the Deployment through
// the owner reference set below, not through this reconciler.
//
// The second return value reports whether the Deployment found at the
// target name is not controlled by subnet (no matching owner reference).
// When true, the returned Deployment is left exactly as found: it is
// never adopted or overwritten even though its name matches.
func (r *SubnetReconciler) reconcileBootdDeployment(ctx context.Context, subnet *keziov1alpha3.Subnet) (dep *appsv1.Deployment, unowned bool, err error) {
	desired := buildBootdDeployment(subnet, r.BootdDeployment)
	if err := ctrl.SetControllerReference(subnet, desired, r.Scheme); err != nil {
		return nil, false, fmt.Errorf("set owner reference on bootd deployment: %w", err)
	}

	existing := &appsv1.Deployment{}
	getErr := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	switch {
	case kerrors.IsNotFound(getErr):
		if err := r.Create(ctx, desired); err != nil && !kerrors.IsAlreadyExists(err) {
			return nil, false, fmt.Errorf("create bootd deployment: %w", err)
		}
		return desired, false, nil
	case getErr != nil:
		return nil, false, fmt.Errorf("get bootd deployment: %w", getErr)
	case !metav1.IsControlledBy(existing, subnet):
		return existing, true, nil
	default:
		if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) || !equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
			existing.Labels = desired.Labels
			existing.Spec = desired.Spec
			if err := r.Update(ctx, existing); err != nil {
				return nil, false, fmt.Errorf("update bootd deployment: %w", err)
			}
		}
		return existing, false, nil
	}
}

// checkBootdNamespacePrerequisites checks the two objects that must
// already exist in subnet's own namespace before its bootd pod can start:
// the pod-security.kubernetes.io/enforce=privileged label bootd's
// NET_ADMIN needs, and the ServiceAccount buildBootdDeployment stamps
// unconditionally. Neither is created here - only detected and named.
//
// SetupWithManager registers no Watch for either: a cluster-wide
// Namespace watch and per-namespace ServiceAccount events would run far
// more often than this rare misconfiguration warrants. onChange instead
// returns bootdNamespacePrerequisiteRequeueAfter on a Violation here.
func (r *SubnetReconciler) checkBootdNamespacePrerequisites(ctx context.Context, subnet *keziov1alpha3.Subnet) ([]subnetCheck, error) {
	var checks []subnetCheck

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: subnet.Namespace}, ns); err != nil {
		return nil, fmt.Errorf("get namespace %s: %w", subnet.Namespace, err)
	}
	if ns.Labels["pod-security.kubernetes.io/enforce"] == "privileged" {
		checks = append(checks, subnetCheck{result: nadvalidate.CheckResult{
			Verdict: nadvalidate.OK,
			Reason:  "BootdNamespacePSALabelPresent",
			Message: fmt.Sprintf("namespace %s carries pod-security.kubernetes.io/enforce=privileged", subnet.Namespace),
		}})
	} else {
		checks = append(checks, subnetCheck{result: nadvalidate.CheckResult{
			Verdict: nadvalidate.Violation,
			Reason:  "BootdNamespacePSALabelMissing",
			Message: fmt.Sprintf("namespace %s has no pod-security.kubernetes.io/enforce=privileged label; bootd needs NET_ADMIN, which Pod Security Admission rejects under the default (restricted) profile - if the bootd pod is not starting, this missing label is the likely cause, though a cluster-wide PSA exemption or default configured outside this namespace's own labels could also apply", subnet.Namespace),
		}})
	}

	saName := r.BootdDeployment.serviceAccountName()
	sa := &corev1.ServiceAccount{}
	err := r.Get(ctx, client.ObjectKey{Namespace: subnet.Namespace, Name: saName}, sa)
	switch {
	case err == nil:
		checks = append(checks, subnetCheck{result: nadvalidate.CheckResult{
			Verdict: nadvalidate.OK,
			Reason:  "BootdServiceAccountPresent",
			Message: fmt.Sprintf("ServiceAccount %s/%s exists", subnet.Namespace, saName),
		}})
	case kerrors.IsNotFound(err):
		checks = append(checks, subnetCheck{result: nadvalidate.CheckResult{
			Verdict: nadvalidate.Violation,
			Reason:  "BootdServiceAccountMissing",
			Message: fmt.Sprintf("ServiceAccount %s/%s does not exist; the bootd Deployment stamps serviceAccountName: %s unconditionally, and its pod cannot start without it", subnet.Namespace, saName, saName),
		}})
	default:
		return nil, fmt.Errorf("get serviceaccount %s/%s: %w", subnet.Namespace, saName, err)
	}

	return checks, nil
}

// findBootdNetworkCollision reports another Subnet (if any) whose
// BootdNetworkRef resolves to the same NAD (same namespace after
// resolveNamespace, same name) as subnet's own - the same NAD object
// means the same broadcast domain, regardless of what CIDR or
// bootdServerIP either Subnet declares.
//
// Subnets are listed cluster-wide. Ties are broken by namespace then
// name, so the reported "other party" is deterministic across reconciles.
func (r *SubnetReconciler) findBootdNetworkCollision(ctx context.Context, subnet *keziov1alpha3.Subnet) (*keziov1alpha3.Subnet, error) {
	var subnets keziov1alpha3.SubnetList
	if err := r.List(ctx, &subnets); err != nil {
		return nil, fmt.Errorf("list subnets: %w", err)
	}

	ns := resolveNamespace(*subnet.Spec.BootdNetworkRef, subnet.Namespace)
	name := subnet.Spec.BootdNetworkRef.Name

	var other *keziov1alpha3.Subnet
	for i := range subnets.Items {
		candidate := &subnets.Items[i]
		if candidate.UID == subnet.UID {
			continue
		}
		if candidate.Spec.BootdNetworkRef == nil {
			// No boot half, no bootdNetworkRef to collide on.
			continue
		}
		candidateNS := resolveNamespace(*candidate.Spec.BootdNetworkRef, candidate.Namespace)
		if candidateNS != ns || candidate.Spec.BootdNetworkRef.Name != name {
			continue
		}
		if other == nil || candidate.Namespace < other.Namespace ||
			(candidate.Namespace == other.Namespace && candidate.Name < other.Name) {
			other = candidate
		}
	}
	return other, nil
}

// runNADChecks resolves subnet's BootdNetworkRef (and SeederNetworkRef,
// when set) to NADs in subnet's own namespace and runs
// internal/nadvalidate's checks against them. Seeder checks are skipped
// when SeederNetworkRef is nil.
//
// A NAD that cannot be fetched or parsed produces an Indeterminate
// result rather than aborting the reconcile; only a non-NotFound API
// error, or a failed seeder-demand count, is returned as an error.
//
// SetupWithManager deliberately registers no Watch on
// NetworkAttachmentDefinition: it is a foreign CRD (Multus's), and
// starting an informer for a GVK whose CRD is absent at manager start
// would fail the shared manager entirely. Trade-off: a newly created NAD
// does not requeue its waiting Subnet promptly; recovery waits for the
// manager's default resync or another event touching the Subnet.
func (r *SubnetReconciler) runNADChecks(ctx context.Context, subnet *keziov1alpha3.Subnet) ([]subnetCheck, error) {
	var checks []subnetCheck

	if subnet.Spec.HasBootPlane() {
		bootdConfig, err := fetchNADConfig(ctx, r.Client, *subnet.Spec.BootdNetworkRef, subnet.Namespace)
		if err != nil {
			if !isIndeterminateNADErr(err) {
				return nil, err
			}
			checks = append(checks, subnetCheck{result: indeterminateFromFetchErr("Bootd", "bootd NAD", err)})
		} else {
			checks = append(checks, subnetCheck{result: nadvalidate.CheckBootdAddress(bootdConfig, subnet.Spec.BootdServerIP)})
		}
	}

	if subnet.Spec.SeederNetworkRef == nil {
		return checks, nil
	}

	seederConfig, err := fetchNADConfig(ctx, r.Client, *subnet.Spec.SeederNetworkRef, subnet.Namespace)
	if err != nil {
		if !isIndeterminateNADErr(err) {
			return nil, err
		}
		fetchErr := indeterminateFromFetchErr("Seeder", "seeder NAD", err)
		checks = append(checks, subnetCheck{result: fetchErr})
		return checks, nil
	}
	// CheckSeederOverlap reports its own Indeterminate (reason
	// NoBootdAddress) when subnet.Spec.BootdServerIP is empty, so a
	// Subnet with no boot half still gets a result that explains why
	// there is nothing to check rather than the call being skipped.
	checks = append(checks, subnetCheck{result: nadvalidate.CheckSeederOverlap(seederConfig, subnet.Spec.BootdServerIP)})

	concurrentImages, err := r.concurrentSeederDeployments(ctx, subnet)
	if err != nil {
		return nil, err
	}
	checks = append(checks, subnetCheck{result: nadvalidate.CheckSeederStaticMultiImage(seederConfig, concurrentImages)})

	return checks, nil
}

// checkSiteRef reports whether subnet.Spec.SiteRef still names a Site that
// exists. The webhook (SubnetCustomValidator.validateSiteRef) enforces
// this at admission; a Site can be deleted afterward, leaving the
// reference dangling with nothing else to surface it, since a dangling
// siteRef changes no field Owns/the bootd Deployment watch would ever see.
// SetupWithManager's Site watch (mapSiteToSubnets) is what gets this
// Subnet reconciled again when that happens.
//
// Never blocking: subnet's own bootd Deployment does not depend on the
// Site existing, only Site-scoped features (seeder/tracker placement) do.
func (r *SubnetReconciler) checkSiteRef(ctx context.Context, subnet *keziov1alpha3.Subnet) (subnetCheck, error) {
	ref := subnet.Spec.SiteRef
	ns := resolveNamespace(ref, subnet.Namespace)

	site := &keziov1alpha3.Site{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, site)
	switch {
	case err == nil:
		return subnetCheck{result: nadvalidate.CheckResult{
			Verdict: nadvalidate.OK,
			Reason:  "SiteFound",
			Message: fmt.Sprintf("Site %s/%s exists", ns, ref.Name),
		}}, nil
	case kerrors.IsNotFound(err):
		return subnetCheck{result: nadvalidate.CheckResult{
			Verdict: nadvalidate.Violation,
			Reason:  "SiteNotFound",
			Message: fmt.Sprintf("spec.siteRef names Site %s/%s, which does not exist", ns, ref.Name),
		}}, nil
	default:
		return subnetCheck{}, fmt.Errorf("get site %s/%s: %w", ns, ref.Name, err)
	}
}

// fetchNADConfig fetches the NAD ref names (in defaultNS when ref carries
// no namespace of its own) through c and returns its spec.config. Shared
// by SubnetReconciler and SiteReconciler, both of which resolve a NAD ref
// to a config string before handing it to internal/nadvalidate. A failure
// to read spec.config out of an otherwise-fetched NAD is wrapped in
// nadContentError so callers can tell it apart from a Get failure.
func fetchNADConfig(ctx context.Context, c client.Client, ref keziov1alpha3.NameRef, defaultNS string) (string, error) {
	ns := resolveNamespace(ref, defaultNS)
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(networkAttachmentDefinitionGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, nad); err != nil {
		return "", err
	}
	config, err := nadvalidate.ConfigFromUnstructured(nad)
	if err != nil {
		return "", nadContentError{err: err}
	}
	return config, nil
}

// nadContentError marks a fetchNADConfig failure that happened after a
// successful Get: the NAD exists but its spec.config is missing or
// unreadable. Always Indeterminate, never a reason to requeue.
type nadContentError struct{ err error }

func (e nadContentError) Error() string { return e.err.Error() }
func (e nadContentError) Unwrap() error { return e.err }

// isIndeterminateNADErr reports whether a fetchNADConfig error may be
// folded into an Indeterminate condition: the NAD does not exist, its
// kind does not exist in this cluster at all (no NAD CRD installed - a
// cluster with no Multus), or its config could not be read. Any other
// error must be returned up so Reconcile requeues with backoff.
//
// A cluster with no NetworkAttachmentDefinition CRD is a different error
// shape from a missing NAD object: the client can't resolve the GVK to a
// REST resource at all, so it returns a RESTMapper error
// (apimeta.IsNoMatchError), not kerrors.IsNotFound. Treating that as
// transient (the pre-fix behavior) means Reconcile returns it as an
// error forever and Valid/Ready are never written - see NADKindAbsent
// below.
func isIndeterminateNADErr(err error) bool {
	if kerrors.IsNotFound(err) {
		return true
	}
	if apimeta.IsNoMatchError(err) {
		return true
	}
	var contentErr nadContentError
	return errors.As(err, &contentErr)
}

// indeterminateFromFetchErr turns an isIndeterminateNADErr-accepted
// failure into the Indeterminate shape nadvalidate's own checks use.
// reasonPrefix ("Bootd" or "Seeder") keeps Reason a CamelCase token in
// the same family as nadvalidate's own.
func indeterminateFromFetchErr(reasonPrefix, what string, err error) nadvalidate.CheckResult {
	reason := reasonPrefix + "NADUnresolved"
	switch {
	case kerrors.IsNotFound(err):
		reason = reasonPrefix + "NADNotFound"
	case apimeta.IsNoMatchError(err):
		reason = reasonPrefix + "NADKindAbsent"
	}
	return nadvalidate.CheckResult{
		Verdict: nadvalidate.Indeterminate,
		Reason:  reason,
		Message: fmt.Sprintf("%s: %v", what, err),
	}
}

// concurrentSeederDeployments counts the per-(Image, Site) seeder
// Deployments ImageReconciler placed on subnet's own seeder network
// (partitionContentSeederSubnetLabel, scoped to subnet's namespace since
// that label carries a bare Subnet name) that currently have an available
// replica. It is CheckSeederStaticMultiImage's concurrentImages input:
// this Subnet's own NAD is what a static multi-image address pool
// actually constrains, so only seeders sharing it count.
func (r *SubnetReconciler) concurrentSeederDeployments(ctx context.Context, subnet *keziov1alpha3.Subnet) (int, error) {
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(subnet.Namespace), client.MatchingLabels{
		partitionContentAppComponentLabel: partitionContentSeederComponentValue,
		partitionContentSeederSubnetLabel: subnet.Name,
	}); err != nil {
		return 0, fmt.Errorf("list seeder deployments: %w", err)
	}
	count := 0
	for i := range deployments.Items {
		if deployments.Items[i].Status.AvailableReplicas > 0 {
			count++
		}
	}
	return count, nil
}

// updateSubnetConditions writes SubnetConditionValid and
// SubnetConditionReady from checks and the bootd Deployment's own
// configuration/availability.
//
// Valid is False on any check's Violation (an unfixed misconfiguration,
// blocking or not), Unknown on an Indeterminate with no Violation
// present, and True otherwise. Ready mirrors the same Violation/
// Indeterminate precedence. When hasBootPlane is true, Ready additionally
// goes False when no bootd Deployment is configured or it has not become
// Available; a Subnet with no boot half hosts no bootd Deployment to wait
// on, so Ready reflects validation alone once hasBootPlane is false -
// depConfigured/depAvailable/depReason/depMessage are then unused.
//
// Within the Violation tier, a blocking check (subnetCheck.blocks) always
// outranks a non-blocking one when choosing which Reason/Message to
// report - it is the one actually withholding the Deployment. Among
// several equally-ranked Violations, the earliest-appended one is
// reported: each independently keeps Ready False, so any pick is a
// truthful explanation, and fixing them one at a time still converges on
// Ready=True.
func (r *SubnetReconciler) updateSubnetConditions(ctx context.Context, subnet *keziov1alpha3.Subnet, checks []subnetCheck, hasBootPlane, depConfigured, depAvailable bool, depReason, depMessage string) error {
	var violation, indeterminate *subnetCheck
	for i := range checks {
		c := &checks[i]
		switch c.result.Verdict {
		case nadvalidate.Violation:
			if violation == nil || (c.blocks && !violation.blocks) {
				violation = c
			}
		case nadvalidate.Indeterminate:
			if indeterminate == nil {
				indeterminate = c
			}
		}
	}

	validStatus, validReason, validMessage := metav1.ConditionTrue, "SubnetValid", "no blocking validation issues found"
	switch {
	case violation != nil:
		validStatus, validReason, validMessage = metav1.ConditionFalse, violation.result.Reason, violation.result.Message
	case indeterminate != nil:
		validStatus, validReason, validMessage = metav1.ConditionUnknown, indeterminate.result.Reason, indeterminate.result.Message
	}
	apimeta.SetStatusCondition(&subnet.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.SubnetConditionValid,
		Status:             validStatus,
		Reason:             validReason,
		Message:            validMessage,
		ObservedGeneration: subnet.Generation,
	})

	readyReason, readyMessage := "BootdReady", "bootd Deployment is available and validation found no issues"
	if !hasBootPlane {
		readyReason, readyMessage = "SubnetReady", "this Subnet carries no boot half; validation found no issues"
	}
	readyStatus := metav1.ConditionTrue
	switch {
	case violation != nil:
		readyStatus, readyReason, readyMessage = metav1.ConditionFalse, violation.result.Reason, violation.result.Message
	case hasBootPlane && (!depConfigured || !depAvailable):
		readyStatus, readyReason, readyMessage = metav1.ConditionFalse, depReason, depMessage
	case indeterminate != nil:
		readyStatus, readyReason, readyMessage = metav1.ConditionUnknown, indeterminate.result.Reason, indeterminate.result.Message
	}
	apimeta.SetStatusCondition(&subnet.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.SubnetConditionReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: subnet.Generation,
	})

	if err := r.Status().Update(ctx, subnet); err != nil {
		return fmt.Errorf("update subnet status: %w", err)
	}
	return nil
}

// mapSiteToSubnets requeues every Subnet in the changed Site's own
// namespace whose spec.siteRef names it. Owner references and Owns' watch
// only ever see this Subnet's own bootd Deployment; a Site going away
// touches no Subnet directly, so nothing else would ever bring this
// Subnet back through the workqueue for checkSiteRef to catch. Mirrors
// SiteReconciler.mapSubnetToSite in the opposite direction.
func (r *SubnetReconciler) mapSiteToSubnets(ctx context.Context, obj client.Object) []reconcile.Request {
	site, ok := obj.(*keziov1alpha3.Site)
	if !ok {
		return nil
	}
	var subnets keziov1alpha3.SubnetList
	if err := r.List(ctx, &subnets, client.InNamespace(site.Namespace)); err != nil {
		return nil
	}
	siteIdentity := sitederive.SiteIdentity(site)
	var requests []reconcile.Request
	for i := range subnets.Items {
		if sitederive.SiteRefIdentity(&subnets.Items[i]) == siteIdentity {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&subnets.Items[i])})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager. Owns
// requeues a Subnet whenever a bootd Deployment it controls changes -
// for example rolling out to Available - so Ready reflects that without
// waiting for the Subnet's own next spec change. Watches Site requeues a
// Subnet when the Site its own siteRef names is created, deleted, or
// otherwise changes (see mapSiteToSubnets) - in particular, a Site
// deletion, which checkSiteRef must surface as Valid=False.
func (r *SubnetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha3.Subnet{}).
		Owns(&appsv1.Deployment{}).
		Watches(&keziov1alpha3.Site{}, handler.EnqueueRequestsFromMapFunc(r.mapSiteToSubnets)).
		Named("subnet").
		Complete(r)
}
