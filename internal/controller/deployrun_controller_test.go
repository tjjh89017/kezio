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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func TestRunsToGC(t *testing.T) {
	ts := func(offsetSeconds int) metav1.Time {
		return metav1.NewTime(time.Unix(1_700_000_000+int64(offsetSeconds), 0))
	}
	run := func(name string, offsetSeconds int) keziov1alpha2.DeployRun {
		return keziov1alpha2.DeployRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: ts(offsetSeconds)},
		}
	}

	cases := []struct {
		name    string
		runs    []keziov1alpha2.DeployRun
		machine keziov1alpha2.Machine
		retain  int
		want    []string // names expected in the GC victim list
	}{
		{
			name:   "fewer runs than retain: nothing is GC'd",
			runs:   []keziov1alpha2.DeployRun{run("r1", 1), run("r2", 2)},
			retain: 5,
			want:   nil,
		},
		{
			name: "keeps the newest N by creationTimestamp, deletes the rest",
			runs: []keziov1alpha2.DeployRun{
				run("r1", 1), run("r2", 2), run("r3", 3),
				run("r4", 4), run("r5", 5), run("r6", 6), run("r7", 7),
			},
			retain: 5,
			want:   []string{"r2", "r1"},
		},
		{
			name: "ties on creationTimestamp break on name, descending",
			runs: []keziov1alpha2.DeployRun{
				run("a", 1), run("b", 1), run("c", 1),
			},
			retain: 1,
			// same timestamp for all three: name descending keeps "c",
			// deletes "b" then "a".
			want: []string{"b", "a"},
		},
		{
			name: "currentRunRef outside the newest N survives",
			runs: []keziov1alpha2.DeployRun{
				run("old", 1), run("r2", 2), run("r3", 3), run("r4", 4), run("r5", 5), run("r6", 6),
			},
			machine: keziov1alpha2.Machine{Status: keziov1alpha2.MachineStatus{
				CurrentRunRef: &keziov1alpha2.NameRef{Name: "old"},
			}},
			retain: 5,
			want:   nil,
		},
		{
			name: "lastSuccessfulRunRef outside the newest N survives, unprotected older runs still go",
			runs: []keziov1alpha2.DeployRun{
				run("oldest", 1), run("old", 2), run("r3", 3), run("r4", 4), run("r5", 5), run("r6", 6), run("r7", 7),
			},
			machine: keziov1alpha2.Machine{Status: keziov1alpha2.MachineStatus{
				LastSuccessfulRunRef: &keziov1alpha2.NameRef{Name: "old"},
			}},
			retain: 5,
			want:   []string{"oldest"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runsToGC(tc.runs, &tc.machine, tc.retain)
			gotNames := make([]string, len(got))
			for i, r := range got {
				gotNames[i] = r.Name
			}
			if len(gotNames) == 0 {
				gotNames = nil
			}
			if fmt.Sprint(gotNames) != fmt.Sprint(tc.want) {
				t.Errorf("runsToGC() victims = %v, want %v", gotNames, tc.want)
			}
		})
	}
}

var _ = Describe("DeployRun Controller", func() {
	Context("Retention GC", func() {
		ctx := context.Background()

		newMachine := func(name string) *keziov1alpha2.Machine {
			m := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: fmt.Sprintf("aa:bb:cc:dd:%02x:%02x", GinkgoRandomSeed()%256, (GinkgoRandomSeed()/256)%256),
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			return m
		}

		newRun := func(name string, machine *keziov1alpha2.Machine) *keziov1alpha2.DeployRun {
			r := &keziov1alpha2.DeployRun{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec:       keziov1alpha2.DeployRunSpec{MachineRef: keziov1alpha2.NameRef{Name: machine.Name}},
			}
			Expect(k8sClient.Create(ctx, r)).To(Succeed())
			return r
		}

		remainingNames := func(machine *keziov1alpha2.Machine) []string {
			var list keziov1alpha2.DeployRunList
			Expect(k8sClient.List(ctx, &list, client.InNamespace("default"))).To(Succeed())
			var names []string
			for _, r := range list.Items {
				if r.Spec.MachineRef.Name == machine.Name {
					names = append(names, r.Name)
				}
			}
			return names
		}

		It("keeps the newest 5 DeployRuns and deletes older ones, leaving other machines untouched", func() {
			machineA := newMachine(fmt.Sprintf("gc-a-%d", GinkgoRandomSeed()))
			machineB := newMachine(fmt.Sprintf("gc-b-%d", GinkgoRandomSeed()))
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, machineA)).To(Succeed())
				Expect(k8sClient.Delete(ctx, machineB)).To(Succeed())
			})

			otherRun := newRun(fmt.Sprintf("%s-only", machineB.Name), machineB)

			var runs []*keziov1alpha2.DeployRun
			for i := 0; i < 7; i++ {
				runs = append(runs, newRun(fmt.Sprintf("%s-run-%d", machineA.Name, i), machineA))
				// creationTimestamp has one-second resolution; space the
				// creates out so ordering is unambiguous without relying on
				// the name tiebreak.
				time.Sleep(1100 * time.Millisecond)
			}

			reconciler := &DeployRunReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: runs[len(runs)-1].Name, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(remainingNames(machineA)).To(ConsistOf(
				runs[2].Name, runs[3].Name, runs[4].Name, runs[5].Name, runs[6].Name,
			))

			By("leaving the other machine's DeployRun untouched")
			var stillThere keziov1alpha2.DeployRun
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: otherRun.Name, Namespace: "default"}, &stillThere)).To(Succeed())
		})

		It("keeps a protected run outside the newest 5 window", func() {
			machine := newMachine(fmt.Sprintf("gc-protected-%d", GinkgoRandomSeed()))
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
			})

			protectedRun := newRun(fmt.Sprintf("%s-protected", machine.Name), machine)
			time.Sleep(1100 * time.Millisecond)

			patch := client.MergeFrom(machine.DeepCopy())
			machine.Status.LastSuccessfulRunRef = &keziov1alpha2.NameRef{Name: protectedRun.Name}
			Expect(k8sClient.Status().Patch(ctx, machine, patch)).To(Succeed())

			var newest *keziov1alpha2.DeployRun
			for i := 0; i < 5; i++ {
				newest = newRun(fmt.Sprintf("%s-run-%d", machine.Name, i), machine)
			}

			reconciler := &DeployRunReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: newest.Name, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(remainingNames(machine)).To(HaveLen(6), "5 newest plus the protected run outside the window")
			Expect(remainingNames(machine)).To(ContainElement(protectedRun.Name))
		})
	})

	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		deployrun := &keziov1alpha2.DeployRun{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind DeployRun")
			err := k8sClient.Get(ctx, typeNamespacedName, deployrun)
			if err != nil && errors.IsNotFound(err) {
				resource := &keziov1alpha2.DeployRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: keziov1alpha2.DeployRunSpec{
						MachineRef: keziov1alpha2.NameRef{Name: "test-machine"},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &keziov1alpha2.DeployRun{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance DeployRun")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource, with no machine to protect against and nothing to GC", func() {
			By("Reconciling the created resource")
			controllerReconciler := &DeployRunReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
