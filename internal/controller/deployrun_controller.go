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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// retainedRunsPerMachine is how many of a Machine's newest DeployRuns
// survive GC, on top of any protected run outside that window.
const retainedRunsPerMachine = 5

// DeployRunReconciler reconciles a DeployRun object
type DeployRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=deployruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=deployruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=deployruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get

// Reconcile garbage-collects a Machine's older DeployRuns, keeping the
// newest retainedRunsPerMachine plus status.currentRunRef/
// lastSuccessfulRunRef even when either falls outside that window: deleting
// the last successful run would manufacture the provisioning trigger's
// missing-run redeploy case, and deleting the current run would fabricate a
// provisioning failure (the Machine reconciler's own job to report).
func (r *DeployRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var run keziov1alpha2.DeployRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	machineNamespace := run.Spec.MachineRef.Namespace
	if machineNamespace == "" {
		machineNamespace = run.Namespace
	}
	var machine keziov1alpha2.Machine
	if err := r.Get(ctx, client.ObjectKey{Namespace: machineNamespace, Name: run.Spec.MachineRef.Name}, &machine); err != nil {
		// A missing Machine leaves nothing to protect and no per-machine
		// retention to enforce; its DeployRuns are already owner-ref GC'd.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var runList keziov1alpha2.DeployRunList
	if err := r.List(ctx, &runList, client.InNamespace(run.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing DeployRuns in namespace %q: %w", run.Namespace, err)
	}

	var siblings []keziov1alpha2.DeployRun
	for _, item := range runList.Items {
		if item.Spec.MachineRef.Name == machine.Name {
			siblings = append(siblings, item)
		}
	}

	for _, victim := range runsToGC(siblings, &machine, retainedRunsPerMachine) {
		if err := r.Delete(ctx, &victim); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting DeployRun %q: %w", victim.Name, err)
		}
		log.Info("garbage collected DeployRun", "deployRun", victim.Name, "machine", machine.Name)
	}

	return ctrl.Result{}, nil
}

// runsToGC picks the runs to delete out of a single Machine's DeployRuns:
// every run beyond the newest retain, except a run named by
// machine.status.currentRunRef or lastSuccessfulRunRef. A protected run
// outside the retained window still counts toward nothing - it simply
// survives alongside the newest retain, so the kept count can exceed retain
// when a protected run is old.
func runsToGC(runs []keziov1alpha2.DeployRun, machine *keziov1alpha2.Machine, retain int) []keziov1alpha2.DeployRun {
	protected := protectedRunNames(machine)

	ordered := make([]keziov1alpha2.DeployRun, len(runs))
	copy(ordered, runs)
	sort.Slice(ordered, func(i, j int) bool {
		ti, tj := ordered[i].CreationTimestamp, ordered[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return tj.Before(&ti) // newest first
		}
		// GenerateName'd runs created within the same wall-clock second
		// share a creationTimestamp; name is the deterministic tiebreak.
		return ordered[i].Name > ordered[j].Name
	})

	victims := make([]keziov1alpha2.DeployRun, 0, max(len(ordered)-retain, 0))
	for i, candidate := range ordered {
		if i < retain {
			continue
		}
		if protected[candidate.Name] {
			continue
		}
		victims = append(victims, candidate)
	}
	return victims
}

func protectedRunNames(machine *keziov1alpha2.Machine) map[string]bool {
	protected := make(map[string]bool, 2)
	if machine.Status.CurrentRunRef != nil {
		protected[machine.Status.CurrentRunRef.Name] = true
	}
	if machine.Status.LastSuccessfulRunRef != nil {
		protected[machine.Status.LastSuccessfulRunRef.Name] = true
	}
	return protected
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeployRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha2.DeployRun{}).
		Named("deployrun").
		Complete(r)
}
