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

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/nadvalidate"
	"github.com/tjjh89017/kezio/internal/sitederive"
	"github.com/tjjh89017/kezio/internal/subnetvalidate"
)

// Subnet status condition types this reconciler writes, one per
// nadvalidate check, alongside the shared keziov1alpha1.ConditionReady.
const (
	ConditionBootdAddressValid      = "BootdAddressValid"
	ConditionSeederOverlapValid     = "SeederOverlapValid"
	ConditionSeederStaticMultiImage = "SeederStaticMultiImage"
	// ConditionBootdNetworkCollision reports another Subnet whose
	// BootdNetworkRef resolves to the same NAD: two bootd Deployments
	// would both answer every DHCPDISCOVER on that broadcast domain with
	// no way for firmware to prefer one (config/bootd/README.md).
	ConditionBootdNetworkCollision = "BootdNetworkCollision"
	// ConditionBootdDeploymentOwnership reports a Deployment already at
	// this Subnet's bootd Deployment name that this controller does not
	// control (no matching owner reference).
	ConditionBootdDeploymentOwnership = "BootdDeploymentOwnership"
	// ConditionBootdNamespacePSALabel reports whether this Subnet's
	// namespace carries pod-security.kubernetes.io/enforce=privileged,
	// required for bootd's NET_ADMIN capability. A missing label is only
	// a likely cause, not a confirmed denial: PSA also has audit/warn
	// modes and cluster-wide defaults.
	ConditionBootdNamespacePSALabel = "BootdNamespacePSALabel"
	// ConditionBootdServiceAccount reports whether the ServiceAccount
	// buildBootdDeployment stamps as serviceAccountName exists in this
	// Subnet's namespace.
	ConditionBootdServiceAccount = "BootdServiceAccount"
	// ConditionDHCPLeaseRangeValid reports subnetvalidate.CheckDHCPLeaseRange
	// against this Subnet's own DHCP lease range and CIDR. A Violation
	// here also withholds the bootd Deployment (see onChange): a broken
	// lease range means dnsmasq cannot serve leases at all.
	ConditionDHCPLeaseRangeValid = "DHCPLeaseRangeValid"
	// ConditionBootdServerIPInCIDR reports subnetvalidate.CheckBootdServerIPInCIDR
	// against this Subnet's bootdServerIP and CIDR - catching an
	// out-of-segment PXE next-server that ConditionBootdAddressValid
	// cannot, since that check never receives cidr. Runs unconditionally
	// and is in violationBlocks: this is a silent mid-boot timeout for
	// every client on the segment.
	ConditionBootdServerIPInCIDR = "BootdServerIPInCIDR"
	// ConditionSiteRefValid reports whether subnet.Spec.SiteRef resolves
	// to an existing Site (checkSiteRef). Deliberately absent from
	// violationBlocks: bootd serves its broadcast domain regardless of
	// whether the Site edge resolves; only the consumers that walk that
	// edge (concurrentImagesForSubnet, seederDemandBySite, agentserver's
	// buildDeployPlan) are affected.
	ConditionSiteRefValid = "SiteRefValid"
)

// subnetOwnedConditionTypes lists every per-check condition type this
// reconciler writes (excluding the always-written ConditionReady), so
// updateSubnetConditions can prune one that no longer applies this pass
// instead of leaving it frozen at its last verdict. A new condition type
// must be added here too, or it will never be pruned.
var subnetOwnedConditionTypes = []string{
	ConditionBootdAddressValid,
	ConditionSeederOverlapValid,
	ConditionSeederStaticMultiImage,
	ConditionBootdNetworkCollision,
	ConditionBootdNamespacePSALabel,
	ConditionBootdServiceAccount,
	ConditionDHCPLeaseRangeValid,
	ConditionBootdServerIPInCIDR,
	ConditionSiteRefValid,
	ConditionBootdDeploymentOwnership,
}

// bootdDeploymentOwnershipRequeueAfter bounds how long a Subnet with
// ConditionBootdDeploymentOwnership waits before its next reconcile: the
// foreign Deployment has no owner reference back to the Subnet, so Owns'
// watch never sees its deletion.
const bootdDeploymentOwnershipRequeueAfter = time.Minute

// bootdNamespacePrerequisiteRequeueAfter bounds how long a Subnet whose
// namespace fails checkBootdNamespacePrerequisites waits before its next
// reconcile. See that function's doc comment for why this polls instead
// of watching.
const bootdNamespacePrerequisiteRequeueAfter = time.Minute

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
	// NAD validation conditions are still computed and written regardless.
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
	subnet := &keziov1alpha1.Subnet{}
	if err := r.Get(ctx, req.NamespacedName, subnet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !subnet.DeletionTimestamp.IsZero() {
		return r.onDelete(ctx, req, subnet)
	}

	return r.onChange(ctx, req, subnet)
}

// onDelete is a no-op: garbage collection removes the bootd Deployment
// via its owner reference once the API server accepts the delete.
func (r *SubnetReconciler) onDelete(ctx context.Context, _ ctrl.Request, _ *keziov1alpha1.Subnet) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)
	return ctrl.Result{}, nil
}

// onChange ensures subnet's bootd Deployment exists and matches its
// current spec (when r.BootdDeployment is configured), runs
// internal/nadvalidate's checks against subnet's referenced NADs, and
// writes both as status conditions.
//
// A CheckBootdAddress Violation does not withhold the Deployment: bootd
// still answers proxyDHCP/TFTP/the MAC gate correctly regardless of the
// address mismatch, and withholding the Deployment would break every
// other machine on the segment too. It is instead surfaced by failing
// ConditionReady.
func (r *SubnetReconciler) onChange(ctx context.Context, _ ctrl.Request, subnet *keziov1alpha1.Subnet) (ctrl.Result, error) {
	siteRefCheck, err := r.checkSiteRef(ctx, subnet)
	if err != nil {
		return ctrl.Result{}, err
	}

	checks, err := r.runNADChecks(ctx, subnet)
	if err != nil {
		return ctrl.Result{}, err
	}
	checks = append(checks, siteRefCheck)

	bootdServerIPResult := subnetvalidate.CheckBootdServerIPInCIDR(subnet.Spec.CIDR, subnet.Spec.BootdServerIP)
	bootdServerIPViolation := bootdServerIPResult.Verdict == nadvalidate.Violation
	checks = append(checks, nadCheck{ConditionBootdServerIPInCIDR, bootdServerIPResult})

	var leaseRangeViolation bool
	if subnet.Spec.DHCP.Mode == keziov1alpha1.SubnetDHCPModeLease {
		leaseRangeResult := subnetvalidate.CheckDHCPLeaseRange(subnet.Spec.CIDR, subnet.Spec.DHCP.LeaseRangeStart, subnet.Spec.DHCP.LeaseRangeEnd)
		leaseRangeViolation = leaseRangeResult.Verdict == nadvalidate.Violation
		checks = append(checks, nadCheck{ConditionDHCPLeaseRangeValid, leaseRangeResult})
	}

	collidingSubnet, err := r.findBootdNetworkCollision(ctx, subnet)
	if err != nil {
		return ctrl.Result{}, err
	}
	if collidingSubnet != nil {
		checks = append(checks, nadCheck{ConditionBootdNetworkCollision, nadvalidate.CheckResult{
			Verdict: nadvalidate.Violation,
			Reason:  "BootdNetworkCollision",
			// Named as resolved, not as declared, since BootdNetworkRef.Namespace
			// can cross namespaces (findBootdNetworkCollision).
			Message: fmt.Sprintf("Subnet %s/%s also targets bootdNetworkRef %s/%s - two Subnets on the same broadcast domain would each run their own bootd, both answering every DHCPDISCOVER with no way for firmware to prefer one", collidingSubnet.Namespace, collidingSubnet.Name, keziov1alpha1.ResolveNamespace(subnet.Spec.BootdNetworkRef, subnet.Namespace), subnet.Spec.BootdNetworkRef.Name),
		}})
	}

	var depAvailable bool
	var depReason, depMessage string
	var requeueAfter time.Duration
	switch {
	case collidingSubnet != nil:
		// ConditionBootdNetworkCollision above already fails ConditionReady;
		// withhold the Deployment so this Subnet doesn't become the second
		// live responder itself.
	case leaseRangeViolation:
		// dnsmasq is the segment's sole DHCP authority in lease mode and
		// cannot serve a broken range; withhold the Deployment.
	case bootdServerIPViolation:
		// An out-of-cidr bootdServerIP is a silent mid-boot PXE timeout for
		// the whole segment; withhold the Deployment.
	case r.BootdDeployment.enabled():
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
			checks = append(checks, nadCheck{ConditionBootdDeploymentOwnership, nadvalidate.CheckResult{
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
		depReason, depMessage = "BootdDeploymentDisabled", "bootd Deployment reconciliation is not configured (BootdDeploymentConfig.Image is empty)"
	}

	if err := r.updateSubnetConditions(ctx, subnet, checks, depAvailable, depReason, depMessage); err != nil {
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
func (r *SubnetReconciler) reconcileBootdDeployment(ctx context.Context, subnet *keziov1alpha1.Subnet) (dep *appsv1.Deployment, unowned bool, err error) {
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
// already exist in subnet's own namespace before its bootd pod can start
// (config/bootd/README.md's "What you still provide, per Subnet"): the
// pod-security.kubernetes.io/enforce=privileged label bootd's NET_ADMIN
// needs, and the ServiceAccount buildBootdDeployment stamps
// unconditionally. Neither is created here - only detected and named.
//
// SetupWithManager registers no Watch for either: a cluster-wide
// Namespace watch and per-namespace ServiceAccount events would run far
// more often than this rare misconfiguration warrants. onChange instead
// returns bootdNamespacePrerequisiteRequeueAfter on a Violation here.
func (r *SubnetReconciler) checkBootdNamespacePrerequisites(ctx context.Context, subnet *keziov1alpha1.Subnet) ([]nadCheck, error) {
	var checks []nadCheck

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: subnet.Namespace}, ns); err != nil {
		return nil, fmt.Errorf("get namespace %s: %w", subnet.Namespace, err)
	}
	if ns.Labels["pod-security.kubernetes.io/enforce"] == "privileged" {
		checks = append(checks, nadCheck{ConditionBootdNamespacePSALabel, nadvalidate.CheckResult{
			Verdict: nadvalidate.OK,
			Reason:  "BootdNamespacePSALabelPresent",
			Message: fmt.Sprintf("namespace %s carries pod-security.kubernetes.io/enforce=privileged", subnet.Namespace),
		}})
	} else {
		checks = append(checks, nadCheck{ConditionBootdNamespacePSALabel, nadvalidate.CheckResult{
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
		checks = append(checks, nadCheck{ConditionBootdServiceAccount, nadvalidate.CheckResult{
			Verdict: nadvalidate.OK,
			Reason:  "BootdServiceAccountPresent",
			Message: fmt.Sprintf("ServiceAccount %s/%s exists", subnet.Namespace, saName),
		}})
	case kerrors.IsNotFound(err):
		checks = append(checks, nadCheck{ConditionBootdServiceAccount, nadvalidate.CheckResult{
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
// ResolveNamespace, same name) as subnet's own - the same NAD object
// means the same broadcast domain, regardless of what CIDR or
// bootdServerIP either Subnet declares.
//
// Subnets are listed cluster-wide. Ties are broken by namespace then
// name, so the reported "other party" is deterministic across reconciles.
func (r *SubnetReconciler) findBootdNetworkCollision(ctx context.Context, subnet *keziov1alpha1.Subnet) (*keziov1alpha1.Subnet, error) {
	var subnets keziov1alpha1.SubnetList
	if err := r.List(ctx, &subnets); err != nil {
		return nil, fmt.Errorf("list subnets: %w", err)
	}

	ns := keziov1alpha1.ResolveNamespace(subnet.Spec.BootdNetworkRef, subnet.Namespace)
	name := subnet.Spec.BootdNetworkRef.Name

	var other *keziov1alpha1.Subnet
	for i := range subnets.Items {
		candidate := &subnets.Items[i]
		if candidate.UID == subnet.UID {
			continue
		}
		candidateNS := keziov1alpha1.ResolveNamespace(candidate.Spec.BootdNetworkRef, candidate.Namespace)
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

// checkSiteRef resolves subnet.Spec.SiteRef and reports whether it names
// a Site that actually exists. A dangling reference is a Violation, not
// an Indeterminate: it is confirmed wrong, not merely unconfirmable.
//
// A non-NotFound Get error is returned so Reconcile requeues with
// backoff instead of writing a status this package cannot stand behind.
func (r *SubnetReconciler) checkSiteRef(ctx context.Context, subnet *keziov1alpha1.Subnet) (nadCheck, error) {
	ref := subnet.Spec.SiteRef
	ns := keziov1alpha1.ResolveNamespace(ref, subnet.Namespace)
	site := &keziov1alpha1.Site{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, site)
	switch {
	case err == nil:
		return nadCheck{ConditionSiteRefValid, nadvalidate.CheckResult{
			Verdict: nadvalidate.OK,
			Reason:  "SiteRefResolved",
			Message: fmt.Sprintf("siteRef resolves to Site %s/%s", ns, ref.Name),
		}}, nil
	case kerrors.IsNotFound(err):
		return nadCheck{ConditionSiteRefValid, nadvalidate.CheckResult{
			Verdict: nadvalidate.Violation,
			Reason:  "SiteRefNotFound",
			Message: fmt.Sprintf("siteRef names Site %s/%s, which does not exist", ns, ref.Name),
		}}, nil
	default:
		return nadCheck{}, fmt.Errorf("get site %s/%s for subnet %s/%s: %w", ns, ref.Name, subnet.Namespace, subnet.Name, err)
	}
}

// nadCheck pairs one internal/nadvalidate CheckResult with the Subnet
// condition Type it becomes.
type nadCheck struct {
	conditionType string
	result        nadvalidate.CheckResult
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
// manager's default resync (~10h) or another event touching the Subnet.
func (r *SubnetReconciler) runNADChecks(ctx context.Context, subnet *keziov1alpha1.Subnet) ([]nadCheck, error) {
	var checks []nadCheck

	bootdConfig, err := r.fetchNADConfig(ctx, subnet.Spec.BootdNetworkRef, subnet.Namespace)
	if err != nil {
		if !isIndeterminateNADErr(err) {
			return nil, err
		}
		checks = append(checks, nadCheck{ConditionBootdAddressValid, indeterminateFromFetchErr("Bootd", "bootd NAD", err)})
	} else {
		checks = append(checks, nadCheck{ConditionBootdAddressValid, nadvalidate.CheckBootdAddress(bootdConfig, subnet.Spec.BootdServerIP)})
	}

	if subnet.Spec.SeederNetworkRef == nil {
		return checks, nil
	}

	seederConfig, err := r.fetchNADConfig(ctx, *subnet.Spec.SeederNetworkRef, subnet.Namespace)
	if err != nil {
		if !isIndeterminateNADErr(err) {
			return nil, err
		}
		fetchErr := indeterminateFromFetchErr("Seeder", "seeder NAD", err)
		checks = append(checks, nadCheck{ConditionSeederOverlapValid, fetchErr})
		checks = append(checks, nadCheck{ConditionSeederStaticMultiImage, fetchErr})
		return checks, nil
	}
	checks = append(checks, nadCheck{ConditionSeederOverlapValid, nadvalidate.CheckSeederOverlap(seederConfig, subnet.Spec.BootdServerIP)})

	concurrentImages, err := r.concurrentImagesForSubnet(ctx, subnet)
	if err != nil {
		return nil, err
	}
	checks = append(checks, nadCheck{ConditionSeederStaticMultiImage, nadvalidate.CheckSeederStaticMultiImage(seederConfig, concurrentImages)})

	return checks, nil
}

// fetchNADConfig fetches the NAD ref names (in subnet's own namespace)
// and returns its spec.config. A failure to read spec.config out of an
// otherwise-fetched NAD is wrapped in nadContentError so callers can
// tell it apart from a Get failure.
func (r *SubnetReconciler) fetchNADConfig(ctx context.Context, ref keziov1alpha1.NameRef, defaultNS string) (string, error) {
	ns := keziov1alpha1.ResolveNamespace(ref, defaultNS)
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(networkAttachmentDefinitionGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, nad); err != nil {
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
// folded into an Indeterminate condition: the NAD does not exist, or its
// config could not be read. Any other error must be returned up so
// Reconcile requeues with backoff.
func isIndeterminateNADErr(err error) bool {
	if kerrors.IsNotFound(err) {
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
	if kerrors.IsNotFound(err) {
		reason = reasonPrefix + "NADNotFound"
	}
	return nadvalidate.CheckResult{
		Verdict: nadvalidate.Indeterminate,
		Reason:  reason,
		Message: fmt.Sprintf("%s: %v", what, err),
	}
}

// concurrentImagesForSubnet counts how many distinct Images currently
// run a seeder Deployment at subnet's own Site, from the seeder
// Deployments reconcileSeederDeployments already maintains. Deployments
// are listed cluster-wide since a per-Image seeder Deployment lives in
// its Image's namespace, not necessarily subnet's.
//
// A Subnet whose SiteRef does not resolve is logged and treated as zero
// concurrent Images rather than failing the whole reconcile.
func (r *SubnetReconciler) concurrentImagesForSubnet(ctx context.Context, subnet *keziov1alpha1.Subnet) (int, error) {
	siteRef := subnet.Spec.SiteRef
	ns := keziov1alpha1.ResolveNamespace(siteRef, subnet.Namespace)
	site := &keziov1alpha1.Site{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: siteRef.Name}, site); err != nil {
		if kerrors.IsNotFound(err) {
			logf.FromContext(ctx).Error(err, "subnet's siteRef does not resolve; treating seeder concurrency as zero",
				"subnet", client.ObjectKeyFromObject(subnet))
			return 0, nil
		}
		return 0, fmt.Errorf("get site %s/%s for subnet %s/%s: %w", ns, siteRef.Name, subnet.Namespace, subnet.Name, err)
	}

	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.MatchingLabels{AppComponentLabel: SeederComponentValue}); err != nil {
		return 0, fmt.Errorf("list seeder deployments: %w", err)
	}
	siteIdentity := sitederive.Identity(site)
	images := map[string]struct{}{}
	for i := range deployments.Items {
		dep := &deployments.Items[i]
		if dep.Annotations[SeederDeploymentSiteAnnotation] != siteIdentity {
			continue
		}
		images[dep.Labels[SeederDeploymentImageLabel]] = struct{}{}
	}
	return len(images), nil
}

// updateSubnetConditions writes one status condition per check in
// checks, then derives and writes keziov1alpha1.ConditionReady from
// their verdicts and the bootd Deployment's own availability.
//
// Precedence, most severe first: any Violation makes ConditionReady
// False. Otherwise, the bootd Deployment not yet Available (or not
// configured) also makes it False. Otherwise, any Indeterminate check
// makes it Unknown - never collapsed into True (would hide a real blind
// spot) or False (would claim certainty this reconciler lacks). Only
// when every check is OK/Advisory and the Deployment is Available does
// it become True.
//
// Within the Violation tier, violationBlocks breaks ties: a Violation
// that withholds the Deployment outright always outranks one that does
// not. Among multiple blocking Violations, the earliest-appended one is
// reported - each independently withholds the Deployment, so any pick is
// a truthful explanation, and fixing them one at a time still converges
// on ConditionReady=True.
func (r *SubnetReconciler) updateSubnetConditions(ctx context.Context, subnet *keziov1alpha1.Subnet, checks []nadCheck, depAvailable bool, depReason, depMessage string) error {
	var violation, indeterminate *nadCheck

	for i := range checks {
		c := &checks[i]
		status := metav1.ConditionTrue
		switch c.result.Verdict {
		case nadvalidate.Violation:
			status = metav1.ConditionFalse
			if violation == nil || (violationBlocks(c.conditionType) && !violationBlocks(violation.conditionType)) {
				violation = c
			}
		case nadvalidate.Indeterminate:
			status = metav1.ConditionUnknown
			if indeterminate == nil {
				indeterminate = c
			}
		case nadvalidate.Advisory, nadvalidate.OK:
			status = metav1.ConditionTrue
		}
		apimeta.SetStatusCondition(&subnet.Status.Conditions, metav1.Condition{
			Type:               c.conditionType,
			Status:             status,
			Reason:             c.result.Reason,
			Message:            c.result.Message,
			ObservedGeneration: subnet.Generation,
		})
	}

	present := make(map[string]bool, len(checks))
	for i := range checks {
		present[checks[i].conditionType] = true
	}
	for _, t := range subnetOwnedConditionTypes {
		if !present[t] {
			apimeta.RemoveStatusCondition(&subnet.Status.Conditions, t)
		}
	}

	readyStatus := metav1.ConditionTrue
	readyReason, readyMessage := "BootdReady", "bootd Deployment is available and NAD validation found no issues"
	switch {
	case violation != nil:
		readyStatus, readyReason, readyMessage = metav1.ConditionFalse, violation.result.Reason, violation.result.Message
	case !depAvailable:
		readyStatus, readyReason, readyMessage = metav1.ConditionFalse, depReason, depMessage
	case indeterminate != nil:
		readyStatus, readyReason, readyMessage = metav1.ConditionUnknown, indeterminate.result.Reason, indeterminate.result.Message
	}

	apimeta.SetStatusCondition(&subnet.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha1.ConditionReady,
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

// violationBlocks reports whether a Violation on conditionType is one of
// the checks onChange treats as withholding the bootd Deployment
// entirely. SiteRefValid stays non-blocking: bootd never consults the
// Site this Subnet claims, so a dangling SiteRef says nothing about
// whether bootd itself can run. BootdServerIPInCIDR and
// DHCPLeaseRangeValid block because both would otherwise ship a broken
// PXE experience to every client on the segment, unlike a
// BootdAddressValid mismatch which still leaves the rest of the segment
// served correctly.
func violationBlocks(conditionType string) bool {
	switch conditionType {
	case ConditionBootdNetworkCollision, ConditionDHCPLeaseRangeValid, ConditionBootdDeploymentOwnership, ConditionBootdServerIPInCIDR:
		return true
	default:
		return false
	}
}

// SetupWithManager sets up the controller with the Manager. Owns
// requeues a Subnet whenever a bootd Deployment it controls changes -
// for example rolling out to Available - so ConditionReady reflects that
// without waiting for the Subnet's own next spec change.
func (r *SubnetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha1.Subnet{}).
		Owns(&appsv1.Deployment{}).
		Watches(&keziov1alpha1.Site{}, handler.EnqueueRequestsFromMapFunc(r.mapSiteToSubnets)).
		Named("subnet").
		Complete(r)
}

// mapSiteToSubnets requeues every Subnet whose SiteRef resolves to obj,
// a changed Site - notably a Subnet created before its Site, whose
// ConditionSiteRefValid must clear promptly once the Site shows up. A
// Site has no back-reference to the Subnets naming it, so this lists
// Subnets cluster-wide and filters by resolved SiteRef.
func (r *SubnetReconciler) mapSiteToSubnets(ctx context.Context, obj client.Object) []reconcile.Request {
	site, ok := obj.(*keziov1alpha1.Site)
	if !ok {
		return nil
	}

	var subnets keziov1alpha1.SubnetList
	if err := r.List(ctx, &subnets); err != nil {
		logf.FromContext(ctx).Error(err, "list subnets for site watch mapping")
		return nil
	}

	var requests []reconcile.Request
	for i := range subnets.Items {
		subnet := &subnets.Items[i]
		ns := keziov1alpha1.ResolveNamespace(subnet.Spec.SiteRef, subnet.Namespace)
		if ns == site.Namespace && subnet.Spec.SiteRef.Name == site.Name {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(subnet)})
		}
	}
	return requests
}
