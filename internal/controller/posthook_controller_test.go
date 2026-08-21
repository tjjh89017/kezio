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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// posthookTestName returns a distinct PostHook name per seq, so each test
// case gets its own object with no collisions in the shared envtest
// apiserver.
func posthookTestName(seq int) string {
	return fmt.Sprintf("posthook-test-%d", seq)
}

func newTestPostHook(name string) *keziov1alpha2.PostHook {
	return &keziov1alpha2.PostHook{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: keziov1alpha2.PostHookSpec{
			Steps: []keziov1alpha2.PostHookStep{
				{Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepInstallRemovableFallback}},
			},
		},
	}
}

var _ = Describe("PostHook Controller", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	newReconciler := func() *PostHookReconciler {
		return &PostHookReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	It("records Valid=True and Ready=True with a current observedGeneration for a valid PostHook", func() {
		name := posthookTestName(1)
		ph := newTestPostHook(name)
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ph) })

		r := newReconciler()
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.PostHook
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))

		validCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.PostHookConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(validCond.ObservedGeneration).To(Equal(got.Generation))

		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.PostHookConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(readyCond.ObservedGeneration).To(Equal(got.Generation))
	})

	It("records Valid=False and Ready=False with reason InvalidSpec for a spec the admission webhook would also reject", func() {
		// This suite runs the reconciler against envtest's apiserver with
		// no admission webhook installed (see suite_test.go), so a spec
		// the webhook would reject at admission - a restricted builtin
		// with no osFamily - can still be created here, exercising the
		// reconciler's own posthookvalidate.Validate call.
		name := posthookTestName(2)
		ph := newTestPostHook(name)
		ph.Spec.Steps = []keziov1alpha2.PostHookStep{
			{Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepMkswap}},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ph) })

		r := newReconciler()
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.PostHook
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		validCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.PostHookConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(validCond.Reason).To(Equal("InvalidSpec"))

		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.PostHookConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("InvalidSpec"))
	})

	It("records SourceMissing when a step's secretRef names a Secret that does not exist, then flips to True once it is created", func() {
		name := posthookTestName(3)
		ph := newTestPostHook(name)
		ph.Spec.Steps = []keziov1alpha2.PostHookStep{
			{Script: &keziov1alpha2.PostHookScriptSource{SecretRef: &keziov1alpha2.SecretKeyRef{Name: "posthook-test-secret-3", Key: "run.sh"}}},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ph) })

		r := newReconciler()
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var missing keziov1alpha2.PostHook
		Expect(k8sClient.Get(ctx, nn, &missing)).To(Succeed())
		validCond := meta.FindStatusCondition(missing.Status.Conditions, keziov1alpha2.PostHookConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(validCond.Reason).To(Equal("SourceMissing"))
		readyCond := meta.FindStatusCondition(missing.Status.Conditions, keziov1alpha2.PostHookConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("SourceMissing"))

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "posthook-test-secret-3", Namespace: "default"},
			Data:       map[string][]byte{"run.sh": []byte("#!/bin/sh\necho hi\n")},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var resolved keziov1alpha2.PostHook
		Expect(k8sClient.Get(ctx, nn, &resolved)).To(Succeed())
		validCond = meta.FindStatusCondition(resolved.Status.Conditions, keziov1alpha2.PostHookConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue))
		readyCond = meta.FindStatusCondition(resolved.Status.Conditions, keziov1alpha2.PostHookConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("re-evaluates and bumps observedGeneration when spec changes", func() {
		name := posthookTestName(4)
		ph := newTestPostHook(name)
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ph) })

		r := newReconciler()
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var firstPass keziov1alpha2.PostHook
		Expect(k8sClient.Get(ctx, nn, &firstPass)).To(Succeed())
		firstGeneration := firstPass.Status.ObservedGeneration

		firstPass.Spec.Params = []keziov1alpha2.PostHookParam{{Name: "greeting"}}
		Expect(k8sClient.Update(ctx, &firstPass)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var secondPass keziov1alpha2.PostHook
		Expect(k8sClient.Get(ctx, nn, &secondPass)).To(Succeed())
		Expect(secondPass.Generation).To(BeNumerically(">", firstGeneration))
		Expect(secondPass.Status.ObservedGeneration).To(Equal(secondPass.Generation))

		validCond := meta.FindStatusCondition(secondPass.Status.Conditions, keziov1alpha2.PostHookConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.ObservedGeneration).To(Equal(secondPass.Generation))
	})
})
