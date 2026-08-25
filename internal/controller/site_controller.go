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
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/nadvalidate"
)

// trackerDeploymentOwnershipRequeueAfter bounds how long a Site with an
// unowned tracker Deployment name collision waits before its next
// reconcile: the foreign Deployment has no owner reference back to the
// Site, so Owns' watch never sees its deletion. Mirrors
// bootdDeploymentOwnershipRequeueAfter.
const trackerDeploymentOwnershipRequeueAfter = time.Minute

// SiteReconciler reconciles a Site object.
type SiteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// TrackerDeployment configures the per-Site tracker Deployment this
	// reconciler creates and keeps up to date for a Site using
	// Tracker.IP. Its zero value disables Deployment reconciliation
	// entirely (see TrackerDeploymentConfig); status and conditions are
	// still computed and written regardless.
	TrackerDeployment TrackerDeploymentConfig
}

// siteValidationFailure names the first SiteReconciler-side check that
// fails SiteConditionValid: a spec.seederSubnetRef the webhook admitted
// but that has since gone dangling (the Subnet was deleted, or its own
// spec.siteRef was repointed elsewhere).
type siteValidationFailure struct {
	reason  string
	message string
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=sites,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=sites/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=sites/finalizers,verbs=update
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=subnets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile fetches the Site, dispatches to onDelete if it is being
// deleted, and otherwise dispatches to onChange.
//
// Site carries no finalizer: its tracker Deployment always lives in the
// Site's own namespace and is owned by it (reconcileTrackerDeployment), so
// garbage collection removes it once the Site is gone - the same
// reasoning SubnetReconciler's own doc comment gives for bootd.
func (r *SiteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	site := &keziov1alpha2.Site{}
	if err := r.Get(ctx, req.NamespacedName, site); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !site.DeletionTimestamp.IsZero() {
		return r.onDelete(ctx, site)
	}

	return r.onChange(ctx, site)
}

// onDelete is a no-op: garbage collection removes the tracker Deployment
// via its owner reference once the API server accepts the delete.
func (r *SiteReconciler) onDelete(ctx context.Context, _ *keziov1alpha2.Site) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)
	return ctrl.Result{}, nil
}

// onChange resolves site's Subnets and seeder/tracker placement, ensures
// its tracker Deployment exists and matches its current spec when
// eligible, and folds all of it into site's status and Valid/Ready
// conditions.
func (r *SiteReconciler) onChange(ctx context.Context, site *keziov1alpha2.Site) (ctrl.Result, error) {
	subnetRefs, err := r.listSubnetRefs(ctx, site)
	if err != nil {
		return ctrl.Result{}, err
	}
	site.Status.SubnetRefs = subnetRefs

	if site.Spec.SeederSubnetRef == nil {
		// No seeding Subnet: no local seeder and no tracker, by design
		// (SiteSpec.SeederSubnetRef's own doc comment).
		site.Status.SeederReady = false
		site.Status.TrackerURL = ""
		return ctrl.Result{}, r.updateSiteConditions(ctx, site, nil, nil, false, false, false, "", "")
	}

	subnet, invalid, err := r.resolveSeedingSubnet(ctx, site)
	if err != nil {
		return ctrl.Result{}, err
	}
	if invalid != nil {
		site.Status.SeederReady = false
		site.Status.TrackerURL = ""
		return ctrl.Result{}, r.updateSiteConditions(ctx, site, invalid, nil, false, false, false, "", "")
	}

	seederReady, err := r.seederPlacementReady(ctx, subnet)
	if err != nil {
		return ctrl.Result{}, err
	}
	site.Status.SeederReady = seederReady

	if site.Spec.Tracker.ExternalURL != "" {
		// The operator already runs this tracker: kezio creates nothing
		// and checks nothing about its reachability.
		site.Status.TrackerURL = site.Spec.Tracker.ExternalURL
		return ctrl.Result{}, r.updateSiteConditions(ctx, site, nil, nil, false, false, false, "", "")
	}

	site.Status.TrackerURL = trackerAnnounceURL(site.Spec.Tracker.IP)

	if subnet.Spec.SeederNetworkRef == nil {
		// Nothing to single-home the tracker pod's default network on -
		// unlike a seeder (section 3.5), a tracker with no pinned address
		// on some network cannot serve announce traffic at all.
		invalid := &siteValidationFailure{
			reason:  "SeederNetworkRefMissing",
			message: fmt.Sprintf("seeding Subnet %s/%s has no seederNetworkRef, so this Site's tracker cannot be single-homed on a pinned address", subnet.Namespace, subnet.Name),
		}
		return ctrl.Result{}, r.updateSiteConditions(ctx, site, invalid, nil, false, false, false, "", "")
	}

	checks, err := r.runTrackerAddressCheck(ctx, site, subnet)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !r.TrackerDeployment.enabled() {
		return ctrl.Result{}, r.updateSiteConditions(ctx, site, nil, checks, true, false, false,
			"SiteTrackerDeploymentImageUnconfigured",
			"no tracker Deployment image is configured on the manager (TRACKER_DEPLOYMENT_IMAGE); the tracker is not reconciled for this Site")
	}

	dep, unowned, invalid, err := r.reconcileTrackerDeployment(ctx, site, subnet)
	if err != nil {
		return ctrl.Result{}, err
	}
	if invalid != nil {
		return ctrl.Result{}, r.updateSiteConditions(ctx, site, invalid, checks, false, false, false, "", "")
	}

	var requeueAfter time.Duration
	var depAvailable bool
	var depReason, depMessage string
	if unowned {
		depReason = "TrackerDeploymentOwnership"
		depMessage = fmt.Sprintf("a Deployment named %s/%s already exists and is not controlled by this Site - refusing to adopt or overwrite it", dep.Namespace, dep.Name)
		// The foreign Deployment has no owner reference back to site, so
		// Owns' watch never sees its deletion; poll instead.
		requeueAfter = trackerDeploymentOwnershipRequeueAfter
	} else {
		depAvailable = deploymentAvailable(dep)
		depReason, depMessage = trackerDeploymentUnavailableReason(dep)
	}

	if err := r.updateSiteConditions(ctx, site, nil, checks, true, true, depAvailable, depReason, depMessage); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// runTrackerAddressCheck fetches seedingSubnet's SeederNetworkRef config
// and runs nadvalidate.CheckTrackerAddress and
// nadvalidate.CheckSeederStaticWithTracker against site's Tracker.IP, the
// same shape SubnetReconciler.runNADChecks uses for CheckBootdAddress/
// CheckSeederOverlap. Called only once seedingSubnet.Spec.SeederNetworkRef
// is known non-nil, and only for a Site whose tracker kezio manages
// itself (onChange returns earlier for Tracker.ExternalURL).
//
// A NAD that cannot be fetched or parsed produces an Indeterminate result
// rather than aborting the reconcile, mirroring runNADChecks; only a
// non-NotFound API error is returned as an error.
func (r *SiteReconciler) runTrackerAddressCheck(ctx context.Context, site *keziov1alpha2.Site, seedingSubnet *keziov1alpha2.Subnet) ([]nadvalidate.CheckResult, error) {
	seederConfig, err := fetchNADConfig(ctx, r.Client, *seedingSubnet.Spec.SeederNetworkRef, seedingSubnet.Namespace)
	if err != nil {
		if !isIndeterminateNADErr(err) {
			return nil, err
		}
		return []nadvalidate.CheckResult{indeterminateFromFetchErr("Seeder", "seeder NAD", err)}, nil
	}
	return []nadvalidate.CheckResult{
		nadvalidate.CheckTrackerAddress(seedingSubnet.Spec.CIDR, seederConfig, site.Spec.Tracker.IP),
		nadvalidate.CheckSeederStaticWithTracker(seederConfig, site.Spec.Tracker.IP),
	}, nil
}

// listSubnetRefs returns the sorted names of the Subnets in site's own
// namespace whose spec.siteRef names site - SiteStatus.SubnetRefs' exact
// contract.
func (r *SiteReconciler) listSubnetRefs(ctx context.Context, site *keziov1alpha2.Site) ([]string, error) {
	var subnets keziov1alpha2.SubnetList
	if err := r.List(ctx, &subnets, client.InNamespace(site.Namespace)); err != nil {
		return nil, fmt.Errorf("list subnets: %w", err)
	}
	var refs []string
	for i := range subnets.Items {
		if subnets.Items[i].Spec.SiteRef.Name == site.Name {
			refs = append(refs, subnets.Items[i].Name)
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// resolveSeedingSubnet fetches the Subnet site.Spec.SeederSubnetRef names.
// A missing Subnet, or one whose own spec.siteRef no longer names site,
// is reported as a siteValidationFailure rather than an error: the
// webhook (SiteCustomValidator.validateSeederSubnetRef) enforces both at
// admission, so a failure here means the referenced Subnet drifted after
// site was admitted.
func (r *SiteReconciler) resolveSeedingSubnet(ctx context.Context, site *keziov1alpha2.Site) (*keziov1alpha2.Subnet, *siteValidationFailure, error) {
	ref := *site.Spec.SeederSubnetRef
	ns := resolveNamespace(ref, site.Namespace)

	subnet := &keziov1alpha2.Subnet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, subnet); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, &siteValidationFailure{
				reason:  "SeederSubnetNotFound",
				message: fmt.Sprintf("spec.seederSubnetRef names Subnet %s/%s, which does not exist", ns, ref.Name),
			}, nil
		}
		return nil, nil, fmt.Errorf("get seeder subnet %s/%s: %w", ns, ref.Name, err)
	}

	if subnet.Spec.SiteRef.Name != site.Name {
		return nil, &siteValidationFailure{
			reason: "SeederSubnetNotOwned",
			message: fmt.Sprintf(
				"spec.seederSubnetRef names Subnet %s/%s, but that Subnet's spec.siteRef now names %q, not %q: a Site cannot use a Subnet of another Site as its seeding Subnet, because routability is not guaranteed across that line",
				ns, ref.Name, subnet.Spec.SiteRef.Name, site.Name),
		}, nil
	}

	return subnet, nil, nil
}

// seederPlacementReady reports whether a seeder Deployment currently
// targets subnet (partitionContentSeederSubnetLabel and its siblings,
// scoped to subnet's namespace and name, mirroring
// SubnetReconciler.concurrentSeederDeployments) and has at least one
// Available replica. This is SiteStatus.SeederReady's data source: it
// reads the per-(Image, Site) seeder Deployment ImageReconciler already
// builds, rather than building one of its own.
func (r *SiteReconciler) seederPlacementReady(ctx context.Context, subnet *keziov1alpha2.Subnet) (bool, error) {
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(subnet.Namespace), client.MatchingLabels{
		partitionContentAppComponentLabel: partitionContentSeederComponentValue,
		partitionContentSeederSubnetLabel: subnet.Name,
	}); err != nil {
		return false, fmt.Errorf("list seeder deployments: %w", err)
	}
	for i := range deployments.Items {
		if deployments.Items[i].Status.AvailableReplicas > 0 {
			return true, nil
		}
	}
	return false, nil
}

// reconcileTrackerDeployment creates site's tracker Deployment (placed on
// seedingSubnet) if absent, updates it in place when the desired
// Spec/Labels differ, and otherwise leaves it untouched. Deleting the
// Site removes the Deployment through the owner reference set below, not
// through this reconciler. Mirrors
// SubnetReconciler.reconcileBootdDeployment; see its doc comment for the
// unowned-Deployment return value's meaning.
//
// invalid is non-nil, with dep/unowned/err all zero, when
// seedingSubnet.Spec.CIDR does not parse (buildTrackerDeployment): that is
// a Site misconfiguration by way of its seeding Subnet, surfaced the same
// way resolveSeedingSubnet's own failures are, not a create/update error
// worth retrying as one.
func (r *SiteReconciler) reconcileTrackerDeployment(ctx context.Context, site *keziov1alpha2.Site, seedingSubnet *keziov1alpha2.Subnet) (dep *appsv1.Deployment, unowned bool, invalid *siteValidationFailure, err error) {
	desired, err := buildTrackerDeployment(site, seedingSubnet, r.TrackerDeployment)
	if err != nil {
		return nil, false, &siteValidationFailure{
			reason:  "SeederSubnetCIDRUnparseable",
			message: fmt.Sprintf("seeding Subnet %s/%s cidr %q cannot be parsed, so the tracker's pinned address cannot be given the CIDR notation Multus requires: %v", seedingSubnet.Namespace, seedingSubnet.Name, seedingSubnet.Spec.CIDR, err),
		}, nil
	}
	if err := ctrl.SetControllerReference(site, desired, r.Scheme); err != nil {
		return nil, false, nil, fmt.Errorf("set owner reference on tracker deployment: %w", err)
	}

	existing := &appsv1.Deployment{}
	getErr := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	switch {
	case kerrors.IsNotFound(getErr):
		if err := r.Create(ctx, desired); err != nil && !kerrors.IsAlreadyExists(err) {
			return nil, false, nil, fmt.Errorf("create tracker deployment: %w", err)
		}
		return desired, false, nil, nil
	case getErr != nil:
		return nil, false, nil, fmt.Errorf("get tracker deployment: %w", getErr)
	case !metav1.IsControlledBy(existing, site):
		return existing, true, nil, nil
	default:
		if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) || !equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
			existing.Labels = desired.Labels
			existing.Spec = desired.Spec
			if err := r.Update(ctx, existing); err != nil {
				return nil, false, nil, fmt.Errorf("update tracker deployment: %w", err)
			}
		}
		return existing, false, nil, nil
	}
}

// updateSiteConditions writes SiteConditionValid and SiteConditionReady.
//
// Valid is False when invalid names an unresolved seederSubnetRef, or
// when checks (nadvalidate.CheckTrackerAddress's result, when it ran)
// holds a Violation; Unknown when checks holds an Indeterminate and no
// Violation is present; True otherwise. invalid always outranks checks:
// it names a cross-reference that must resolve before a tracker address
// check can even run.
//
// Ready mirrors updateSubnetConditions' own precedence: invalid, then a
// checks Violation, always win (Ready can never be True on top of either);
// otherwise, when a tracker Deployment is wanted (trackerWanted - site
// declares a seeding Subnet and Tracker.IP, not ExternalURL), Ready
// additionally requires depConfigured and depAvailable; then a checks
// Indeterminate goes Unknown. A Site with no tracker Deployment of its own
// (trackerWanted false: no seederSubnetRef, or Tracker.ExternalURL) is
// Ready once Valid, exactly as SiteConditionReady's own doc comment
// states.
func (r *SiteReconciler) updateSiteConditions(ctx context.Context, site *keziov1alpha2.Site, invalid *siteValidationFailure, checks []nadvalidate.CheckResult, trackerWanted, depConfigured, depAvailable bool, depReason, depMessage string) error {
	var violation, indeterminate *nadvalidate.CheckResult
	for i := range checks {
		c := &checks[i]
		switch c.Verdict {
		case nadvalidate.Violation:
			if violation == nil {
				violation = c
			}
		case nadvalidate.Indeterminate:
			if indeterminate == nil {
				indeterminate = c
			}
		}
	}

	validStatus, validReason, validMessage := metav1.ConditionTrue, "SiteValid", "no blocking validation issues found"
	switch {
	case invalid != nil:
		validStatus, validReason, validMessage = metav1.ConditionFalse, invalid.reason, invalid.message
	case violation != nil:
		validStatus, validReason, validMessage = metav1.ConditionFalse, violation.Reason, violation.Message
	case indeterminate != nil:
		validStatus, validReason, validMessage = metav1.ConditionUnknown, indeterminate.Reason, indeterminate.Message
	}
	apimeta.SetStatusCondition(&site.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.SiteConditionValid,
		Status:             validStatus,
		Reason:             validReason,
		Message:            validMessage,
		ObservedGeneration: site.Generation,
	})

	readyStatus, readyReason, readyMessage := metav1.ConditionTrue, "SiteReady", "this Site carries no tracker Deployment of its own; validation found no issues"
	switch {
	case invalid != nil:
		readyStatus, readyReason, readyMessage = metav1.ConditionFalse, invalid.reason, invalid.message
	case violation != nil:
		readyStatus, readyReason, readyMessage = metav1.ConditionFalse, violation.Reason, violation.Message
	case trackerWanted && (!depConfigured || !depAvailable):
		readyStatus, readyReason, readyMessage = metav1.ConditionFalse, depReason, depMessage
	case indeterminate != nil:
		readyStatus, readyReason, readyMessage = metav1.ConditionUnknown, indeterminate.Reason, indeterminate.Message
	case trackerWanted:
		readyReason, readyMessage = "SiteTrackerReady", "tracker Deployment is available and validation found no issues"
	}
	apimeta.SetStatusCondition(&site.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.SiteConditionReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: site.Generation,
	})

	if err := r.Status().Update(ctx, site); err != nil {
		return fmt.Errorf("update site status: %w", err)
	}
	return nil
}

// mapSubnetToSite requeues the Site a changed Subnet's spec.siteRef names,
// in the Subnet's own namespace, so a Subnet create/update that affects
// SiteStatus.SubnetRefs or seeder placement is reflected promptly. A
// Subnet whose siteRef changes away from a Site it used to name is not
// requeued for that former Site; the manager's periodic resync converges
// it eventually, the same trade-off SubnetReconciler accepts for NAD
// watches.
func (r *SiteReconciler) mapSubnetToSite(_ context.Context, obj client.Object) []reconcile.Request {
	subnet, ok := obj.(*keziov1alpha2.Subnet)
	if !ok || subnet.Spec.SiteRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: subnet.Namespace, Name: subnet.Spec.SiteRef.Name}}}
}

// mapSeederDeploymentToSite requeues the Site named in a seeder
// Deployment's imageSeederSiteAnnotation. SiteStatus.SeederReady is
// derived from that Deployment's availability (seederPlacementReady), but
// the Deployment is controlled by the Image, not the Site, so Owns' watch
// never sees it become Available - without this mapping a Site keeps
// reporting seederReady false for as long as nothing else writes the
// Site or one of its Subnets.
func (r *SiteReconciler) mapSeederDeploymentToSite(_ context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok || dep.Labels[partitionContentAppComponentLabel] != partitionContentSeederComponentValue {
		return nil
	}
	ns, name, found := strings.Cut(dep.Annotations[imageSeederSiteAnnotation], "/")
	if !found || ns == "" || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: ns, Name: name}}}
}

// SetupWithManager sets up the controller with the Manager. Owns
// requeues a Site whenever a tracker Deployment it controls changes,
// Watches Subnet requeues it when a Subnet's own siteRef affects this
// Site's status (see mapSubnetToSite), and Watches Deployment requeues it
// when a seeder Deployment it does not control changes (see
// mapSeederDeploymentToSite).
func (r *SiteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha2.Site{}).
		Owns(&appsv1.Deployment{}).
		Watches(&keziov1alpha2.Subnet{}, handler.EnqueueRequestsFromMapFunc(r.mapSubnetToSite)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapSeederDeploymentToSite)).
		Named("site").
		Complete(r)
}
