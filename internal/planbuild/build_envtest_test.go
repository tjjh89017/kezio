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

package planbuild

import (
	"context"
	"crypto/sha1" //nolint:gosec // building a fixture info hash, not a security use
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/posthookdefaults"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/store"
)

// fixtures shares one envtest client across every subtest below, each of
// which works in its own namespace.
type fixtures struct {
	t      *testing.T
	client ctrlclient.Client
}

// TestBuilder_Build_Envtest exercises Builder.Build against a real API
// server, covering every resolution path Build combines: hook attachment
// (default vs explicit), params merge order, disk hint resolution, slot
// classification, and the not-ready/validation error shapes.
func TestBuilder_Build_Envtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" && firstEnvtestBinaryDir() == "" {
		t.Skip("no envtest binaries available (run `make setup-envtest`, or set KUBEBUILDER_ASSETS)")
	}

	testScheme := scheme.Scheme
	if err := keziov1alpha2.AddToScheme(testScheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstEnvtestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	fx := &fixtures{t: t, client: c}

	t.Run("no postHookRefs attaches the default hook from the manager namespace", func(t *testing.T) {
		testDefaultHookAttached(t, fx)
	})
	t.Run("explicit postHookRefs are honored in order, default not attached", func(t *testing.T) {
		testExplicitHookRefsHonored(t, fx)
	})
	t.Run("params merge order is posthook default, then image, then machine", func(t *testing.T) {
		testParamsMergeOrder(t, fx)
	})
	t.Run("ambiguous disk hints fail with a disk-selection error", func(t *testing.T) {
		testAmbiguousDiskHints(t, fx)
	})
	t.Run("unambiguous disk hints select the matching device", func(t *testing.T) {
		testUnambiguousDiskHints(t, fx)
	})
	t.Run("slots classify as mkfs, swap, and torrent", func(t *testing.T) {
		testSlotClassification(t, fx)
	})
	t.Run("a not-Ready partitioncontent is a not-ready error", func(t *testing.T) {
		testPartitionContentNotReady(t, fx)
	})
	t.Run("an unresolved placeholder is a validation error", func(t *testing.T) {
		testUnresolvedPlaceholder(t, fx)
	})
	t.Run("the default hook's efibootmgr step resolves disk/part from the image's ESP slot", func(t *testing.T) {
		testDefaultHookDerivesBuiltinParams(t, fx)
	})
	t.Run("an explicit builtin param overrides the derived default", func(t *testing.T) {
		testBuiltinParamOverride(t, fx)
	})
	t.Run("a builtin needing the ESP fails with a validation error when the image has none", func(t *testing.T) {
		testBuiltinMissingESPIsValidationError(t, fx)
	})
	t.Run("a dataImages-only machine with no postHookRefs attaches no hooks", func(t *testing.T) {
		testDataImagesOnlyNoDefaultHook(t, fx)
	})
	t.Run("a dataImages-only machine with an explicit efibootmgr hook fails with a validation error", func(t *testing.T) {
		testDataImagesOnlyExplicitEfibootmgrIsValidationError(t, fx)
	})
	t.Run("image hooks resolve before machine hooks", func(t *testing.T) {
		testImageHooksResolveBeforeMachineHooks(t, fx)
	})
	t.Run("the default hook is not attached when the image already carries its own hooks", func(t *testing.T) {
		testDefaultHookNotAttachedWhenImageHasHooks(t, fx)
	})
	t.Run("a machine-referenced hook incompatible with the resolved OS image's osFamily is a validation error", func(t *testing.T) {
		testMachineHookIncompatibleOSFamilyIsValidationError(t, fx)
	})
	t.Run("HooksHash changes when the image's own hooks change", func(t *testing.T) {
		testHooksHashChangesWithImageHooks(t, fx)
	})
	t.Run("a configMap-sourced script step resolves and templates", func(t *testing.T) {
		testConfigMapScriptSourceResolvesAndTemplates(t, fx)
	})
	t.Run("a secret-sourced script step resolves and templates", func(t *testing.T) {
		testSecretScriptSourceResolvesAndTemplates(t, fx)
	})
	t.Run("a missing ConfigMap script source is a not-ready error", func(t *testing.T) {
		testConfigMapScriptSourceMissingConfigMapIsNotReady(t, fx)
	})
	t.Run("a missing key in an existing ConfigMap script source is a not-ready error", func(t *testing.T) {
		testConfigMapScriptSourceMissingKeyIsNotReady(t, fx)
	})
	t.Run("a missing Secret script source is a not-ready error", func(t *testing.T) {
		testSecretScriptSourceMissingSecretIsNotReady(t, fx)
	})
	t.Run("a missing key in an existing Secret script source is a not-ready error", func(t *testing.T) {
		testSecretScriptSourceMissingKeyIsNotReady(t, fx)
	})
	t.Run("no seeder deployment yet for the content is a not-ready error", func(t *testing.T) {
		testSeederDeploymentMissingIsNotReady(t, fx)
	})
	t.Run("a seeder pod with no PodIP yet is a not-ready error", func(t *testing.T) {
		testSeederPodNoIPIsNotReady(t, fx)
	})
	t.Run("a missing MachineHardware is a not-ready error", func(t *testing.T) {
		testMachineHardwareMissingIsNotReady(t, fx)
	})
	t.Run("a missing image is a not-ready error", func(t *testing.T) {
		testImageMissingIsNotReady(t, fx)
	})
	t.Run("an image that is not Ready yet is a not-ready error", func(t *testing.T) {
		testImageNotReadyYetIsNotReady(t, fx)
	})
	t.Run("a missing posthook is a not-ready error", func(t *testing.T) {
		testPostHookMissingIsNotReady(t, fx)
	})
	t.Run("a posthook that is not Valid yet is a not-ready error", func(t *testing.T) {
		testPostHookNotValidYetIsNotReady(t, fx)
	})
	t.Run("the OS image and a dataImages entry resolving to the same disk is a disk-selection error", func(t *testing.T) {
		testOSAndDataImageSameDiskIsDiskSelectionError(t, fx)
	})
}

func testImageHooksResolveBeforeMachineHooks(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreatePostHook(ns, "image-hook", scriptHookSpec("from-image"))
	fx.mustCreatePostHook(ns, "machine-hook", scriptHookSpec("from-machine"))
	image := fx.mustCreateImageWithHooks(ns, blankDataLayout(), []keziov1alpha2.NameRef{{Name: "image-hook"}})

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "machine-hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Hooks) != 2 || plan.Hooks[0].Name != "image-hook" || plan.Hooks[1].Name != "machine-hook" {
		t.Fatalf("plan.Hooks = %+v, want [image-hook machine-hook] in that order", plan.Hooks)
	}
}

// testDefaultHookNotAttachedWhenImageHasHooks covers the "substitutes only
// when BOTH lists are empty" rule's other half: the image's own postHookRefs
// being non-empty is enough on its own to suppress the shipped default,
// even though the machine's own postHookRefs stays empty.
func testDefaultHookNotAttachedWhenImageHasHooks(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	mgrNS := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreatePostHook(mgrNS, posthookdefaults.DefaultFinalizeHookName, posthookdefaults.Spec())
	fx.mustCreatePostHook(ns, "image-hook", scriptHookSpec("from-image"))
	image := fx.mustCreateImageWithHooks(ns, blankDataLayout(), []keziov1alpha2.NameRef{{Name: "image-hook"}})

	b := &Builder{Client: fx.client, ManagerNamespace: mgrNS}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec:       keziov1alpha2.MachineSpec{ImageRef: &keziov1alpha2.NameRef{Name: image.Name}},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Hooks) != 1 || plan.Hooks[0].Name != "image-hook" {
		t.Fatalf("plan.Hooks = %+v, want exactly [image-hook], default must not attach", plan.Hooks)
	}
}

func testMachineHookIncompatibleOSFamilyIsValidationError(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreatePostHook(ns, "windows-hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily:     keziov1alpha2.OSFamilyWindows,
			ChrootScript: &keziov1alpha2.PostHookScriptSource{Script: "echo hi"},
		}},
	})
	image := fx.mustCreateImage(ns, blankDataLayout()) // OSFamily defaults to Linux

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "windows-hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("Build err = %v, want a *ValidationError", err)
	}
}

// testHooksHashChangesWithImageHooks pins Snapshot.HooksHash's coverage of
// image-attached hooks: two otherwise-identical Machines deploying images
// that differ only in their own postHookRefs must resolve to different
// hashes, since the resolved hooks driving a deploy actually differ.
func testHooksHashChangesWithImageHooks(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreatePostHook(ns, "image-hook", scriptHookSpec("from-image"))
	fx.mustCreatePostHook(ns, posthookdefaults.DefaultFinalizeHookName, posthookdefaults.Spec())
	withHooks := fx.mustCreateImageWithHooks(ns, blankDataLayout(), []keziov1alpha2.NameRef{{Name: "image-hook"}})
	withoutHooks := fx.mustCreateImageWithHooksNamed(ns, "img2", blankDataLayout(), nil)

	// withoutHooks has no postHookRefs of its own, so Build substitutes the
	// shipped default hook (from the manager namespace) for it - the very
	// difference this test exercises against withHooks' explicit hook.
	b := &Builder{Client: fx.client, ManagerNamespace: ns}
	buildFor := func(image *keziov1alpha2.Image, machineName string) string {
		machine := &keziov1alpha2.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: ns},
			Spec:       keziov1alpha2.MachineSpec{ImageRef: &keziov1alpha2.NameRef{Name: image.Name}},
		}
		fx.mustCreateMachineHardwareNamed(ns, machineName, oneDisk("/dev/vda"))
		run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run-" + machineName, UID: types.UID("uid-" + machineName)}}
		_, snap, err := b.Build(context.Background(), machine, run)
		if err != nil {
			t.Fatalf("Build(%s): %v", machineName, err)
		}
		return snap.HooksHash
	}

	hashWith := buildFor(withHooks, "m-with-hooks")
	hashWithout := buildFor(withoutHooks, "m-without-hooks")
	if hashWith == hashWithout {
		t.Fatalf("HooksHash = %q for both, want different hashes since one machine's image attaches a hook the other's does not", hashWith)
	}
}

func testConfigMapScriptSourceResolvesAndTemplates(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreateConfigMap(ns, "script-cm", map[string]string{"script.sh": "echo {{ .machineName }}"})
	fx.mustCreatePostHook(ns, "cm-hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Script:   &keziov1alpha2.PostHookScriptSource{ConfigMapRef: &keziov1alpha2.ConfigMapKeyRef{Name: "script-cm", Key: "script.sh"}},
		}},
	})
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "cm-hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "echo m1"
	if len(plan.Hooks) != 1 || len(plan.Hooks[0].Steps) != 1 || plan.Hooks[0].Steps[0].Content != want {
		t.Fatalf("rendered content = %+v, want %q", plan.Hooks, want)
	}
}

func testSecretScriptSourceResolvesAndTemplates(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreateSecret(ns, "script-secret", map[string][]byte{"script.sh": []byte("echo {{ .machineName }}")})
	fx.mustCreatePostHook(ns, "secret-hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Script:   &keziov1alpha2.PostHookScriptSource{SecretRef: &keziov1alpha2.SecretKeyRef{Name: "script-secret", Key: "script.sh"}},
		}},
	})
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "secret-hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "echo m1"
	if len(plan.Hooks) != 1 || len(plan.Hooks[0].Steps) != 1 || plan.Hooks[0].Steps[0].Content != want {
		t.Fatalf("rendered content = %+v, want %q", plan.Hooks, want)
	}
}

func testConfigMapScriptSourceMissingConfigMapIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreatePostHook(ns, "cm-hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Script:   &keziov1alpha2.PostHookScriptSource{ConfigMapRef: &keziov1alpha2.ConfigMapKeyRef{Name: "does-not-exist", Key: "script.sh"}},
		}},
	})
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "cm-hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

func testConfigMapScriptSourceMissingKeyIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreateConfigMap(ns, "script-cm", map[string]string{"other-key": "echo hi"})
	fx.mustCreatePostHook(ns, "cm-hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Script:   &keziov1alpha2.PostHookScriptSource{ConfigMapRef: &keziov1alpha2.ConfigMapKeyRef{Name: "script-cm", Key: "script.sh"}},
		}},
	})
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "cm-hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

func testSecretScriptSourceMissingSecretIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreatePostHook(ns, "secret-hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Script:   &keziov1alpha2.PostHookScriptSource{SecretRef: &keziov1alpha2.SecretKeyRef{Name: "does-not-exist", Key: "script.sh"}},
		}},
	})
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "secret-hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

func testSecretScriptSourceMissingKeyIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreateSecret(ns, "script-secret", map[string][]byte{"other-key": []byte("echo hi")})
	fx.mustCreatePostHook(ns, "secret-hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Script:   &keziov1alpha2.PostHookScriptSource{SecretRef: &keziov1alpha2.SecretKeyRef{Name: "script-secret", Key: "script.sh"}},
		}},
	})
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "secret-hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

// testSeederDeploymentMissingIsNotReady covers resolveTorrentURL's first
// not-ready branch: a torrent slot whose content has no seeder Deployment
// yet at all.
func testSeederDeploymentMissingIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))

	hash := fixtureInfoHash("seeder-missing-deployment")
	fx.mustCreatePartitionContentReady(ns, hash)

	layout := keziov1alpha2.ImageDiskLayout{
		PartitionTable: keziov1alpha2.PartitionTableGPT,
		SfdiskJSON:     `{"partitiontable":{}}`,
		Slots: []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: store.ObjectName(hash)}},
		},
	}
	image := fx.mustCreateImage(ns, layout)
	subnetRef, _ := fx.mustCreateSite(ns)

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec:       keziov1alpha2.MachineSpec{ImageRef: &keziov1alpha2.NameRef{Name: image.Name}, SubnetRef: subnetRef},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

// testSeederPodNoIPIsNotReady covers resolveTorrentURL's second not-ready
// branch: the seeder Deployment exists but its Pod has not reported a
// PodIP yet.
func testSeederPodNoIPIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))

	hash := fixtureInfoHash("seeder-no-podip")
	fx.mustCreatePartitionContentReady(ns, hash)

	layout := keziov1alpha2.ImageDiskLayout{
		PartitionTable: keziov1alpha2.PartitionTableGPT,
		SfdiskJSON:     `{"partitiontable":{}}`,
		Slots: []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: store.ObjectName(hash)}},
		},
	}
	image := fx.mustCreateImage(ns, layout)
	subnetRef, siteIdentity := fx.mustCreateSite(ns)
	fx.mustCreateSeederPodNoIP(ns, image.Name, siteIdentity)

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec:       keziov1alpha2.MachineSpec{ImageRef: &keziov1alpha2.NameRef{Name: image.Name}, SubnetRef: subnetRef},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

// testMachineHardwareMissingIsNotReady covers getMachineHardware's
// not-found branch.
func testMachineHardwareMissingIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec:       keziov1alpha2.MachineSpec{ImageRef: &keziov1alpha2.NameRef{Name: image.Name}},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

// testImageMissingIsNotReady covers resolveImage's image-not-found branch.
func testImageMissingIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec:       keziov1alpha2.MachineSpec{ImageRef: &keziov1alpha2.NameRef{Name: "does-not-exist"}},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

// testImageNotReadyYetIsNotReady covers resolveImage's
// image-not-Ready-yet branch: the Image exists but its Status.State has
// not reached ImageStateReady.
func testImageNotReadyYetIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	image := fx.mustCreateImagePending(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec:       keziov1alpha2.MachineSpec{ImageRef: &keziov1alpha2.NameRef{Name: image.Name}},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

// testPostHookMissingIsNotReady covers resolveHook's posthook-not-found
// branch.
func testPostHookMissingIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "does-not-exist"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

// testPostHookNotValidYetIsNotReady covers resolveHook's
// posthook-not-Valid-yet branch: the PostHook exists but its Valid
// condition has not been set True.
func testPostHookNotValidYetIsNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	image := fx.mustCreateImage(ns, blankDataLayout())
	fx.mustCreatePostHookPending(ns, "pending-hook", scriptHookSpec("pending"))

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "pending-hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

// testOSAndDataImageSameDiskIsDiskSelectionError exercises Build's glue
// around diskmatch.CheckDistinct: the OS image and a dataImages entry
// with no disambiguating hints both fall back to "the only disk" on a
// single-disk machine, so they resolve to the same physical disk.
func testOSAndDataImageSameDiskIsDiskSelectionError(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:   &keziov1alpha2.NameRef{Name: image.Name},
			DataImages: []keziov1alpha2.MachineDataImage{{ImageRef: keziov1alpha2.NameRef{Name: image.Name}}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var diskErr *DiskSelectionError
	if !errors.As(err, &diskErr) {
		t.Fatalf("Build err = %v, want a *DiskSelectionError", err)
	}
}

// testDataImagesOnlyNoDefaultHook covers the fast-lane regression: a
// Machine deploying only DataImages (no OS image) must not have the
// shipped default finalize hook substituted in - there is no OS ESP for
// its efibootmgr step to resolve against, and per
// deployer.Deployer.Provision's contract the run simply completes at its
// after-deploy power state with no boot entry to set.
func testDataImagesOnlyNoDefaultHook(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	mgrNS := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreatePostHook(mgrNS, posthookdefaults.DefaultFinalizeHookName, posthookdefaults.Spec())
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client, ManagerNamespace: mgrNS}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			DataImages: []keziov1alpha2.MachineDataImage{{ImageRef: keziov1alpha2.NameRef{Name: image.Name}}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Hooks) != 0 {
		t.Fatalf("plan.Hooks = %+v, want none (the default finalize hook must not attach with no OS image)", plan.Hooks)
	}
}

// testDataImagesOnlyExplicitEfibootmgrIsValidationError covers the flip
// side: an explicit postHookRefs entry is still honored verbatim even on
// a dataImages-only Machine, so a user-attached efibootmgr step with no
// OS ESP to resolve against is still a genuine configuration error.
func testDataImagesOnlyExplicitEfibootmgrIsValidationError(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreatePostHook(ns, "hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Builtin:  &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepEfibootmgr},
		}},
	})
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			DataImages:   []keziov1alpha2.MachineDataImage{{ImageRef: keziov1alpha2.NameRef{Name: image.Name}}},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("Build err = %v, want a *ValidationError", err)
	}
}

func testDefaultHookDerivesBuiltinParams(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	mgrNS := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreatePostHook(mgrNS, posthookdefaults.DefaultFinalizeHookName, posthookdefaults.Spec())
	layout := keziov1alpha2.ImageDiskLayout{
		PartitionTable: keziov1alpha2.PartitionTableGPT,
		SfdiskJSON:     `{"partitiontable":{}}`,
		Slots: []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleESP, FSType: "vfat"},
			{Number: 2, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4"},
		},
	}
	image := fx.mustCreateImage(ns, layout)

	b := &Builder{Client: fx.client, ManagerNamespace: mgrNS}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec:       keziov1alpha2.MachineSpec{ImageRef: &keziov1alpha2.NameRef{Name: image.Name}},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.MachineName != "m1" {
		t.Fatalf("plan.MachineName = %q, want m1", plan.MachineName)
	}
	var efibootmgr *agentapi.ResolvedHookStep
	for _, hook := range plan.Hooks {
		for i, step := range hook.Steps {
			if step.Builtin == keziov1alpha2.BuiltinStepEfibootmgr {
				efibootmgr = &hook.Steps[i]
			}
		}
	}
	if efibootmgr == nil {
		t.Fatalf("plan.Hooks = %+v, want an efibootmgr step", plan.Hooks)
	}
	if efibootmgr.Params["disk"] != "/dev/vda" || efibootmgr.Params["part"] != "1" {
		t.Fatalf("efibootmgr.Params = %+v, want disk=/dev/vda part=1", efibootmgr.Params)
	}
}

func testBuiltinParamOverride(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	layout := keziov1alpha2.ImageDiskLayout{
		PartitionTable: keziov1alpha2.PartitionTableGPT,
		SfdiskJSON:     `{"partitiontable":{}}`,
		Slots: []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleESP, FSType: "vfat"},
			{Number: 2, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4"},
		},
	}
	image := fx.mustCreateImage(ns, layout)
	fx.mustCreatePostHook(ns, "hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Builtin: &keziov1alpha2.PostHookBuiltinStep{
				Name:   keziov1alpha2.BuiltinStepEfibootmgr,
				Params: map[string]string{"part": "9"},
			},
		}},
	})
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Hooks) != 1 || len(plan.Hooks[0].Steps) != 1 {
		t.Fatalf("plan.Hooks = %+v, want exactly one hook with one step", plan.Hooks)
	}
	step := plan.Hooks[0].Steps[0]
	if step.Params["part"] != "9" {
		t.Fatalf("step.Params[part] = %q, want the explicit override 9 (not the derived ESP number 1)", step.Params["part"])
	}
	if step.Params["disk"] != "/dev/vda" {
		t.Fatalf("step.Params[disk] = %q, want the derived default /dev/vda", step.Params["disk"])
	}
}

func testBuiltinMissingESPIsValidationError(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	// blankDataLayout no longer works here for demonstrating the missing-ESP
	// case (it now carries an ESP slot); this layout carries none.
	layout := keziov1alpha2.ImageDiskLayout{
		PartitionTable: keziov1alpha2.PartitionTableGPT,
		SfdiskJSON:     `{"partitiontable":{}}`,
		Slots: []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4"},
		},
	}
	image := fx.mustCreateImage(ns, layout)
	fx.mustCreatePostHook(ns, "hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Builtin:  &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepEfibootmgr},
		}},
	})

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("Build err = %v, want a *ValidationError", err)
	}
}

func testDefaultHookAttached(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	mgrNS := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreatePostHook(mgrNS, posthookdefaults.DefaultFinalizeHookName, posthookdefaults.Spec())
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client, ManagerNamespace: mgrNS}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef: &keziov1alpha2.NameRef{Name: image.Name},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Hooks) != 1 || plan.Hooks[0].Name != posthookdefaults.DefaultFinalizeHookName {
		t.Fatalf("plan.Hooks = %+v, want exactly the default hook", plan.Hooks)
	}
}

func testExplicitHookRefsHonored(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	mgrNS := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	// The default hook exists in the manager namespace too, so a failure
	// to honor explicit refs would silently still pass by falling back to
	// it.
	fx.mustCreatePostHook(mgrNS, posthookdefaults.DefaultFinalizeHookName, posthookdefaults.Spec())
	fx.mustCreatePostHook(ns, "hook-a", scriptHookSpec("a"))
	fx.mustCreatePostHook(ns, "hook-b", scriptHookSpec("b"))
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client, ManagerNamespace: mgrNS}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "hook-a"}, {Name: "hook-b"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Hooks) != 2 || plan.Hooks[0].Name != "hook-a" || plan.Hooks[1].Name != "hook-b" {
		t.Fatalf("plan.Hooks = %+v, want [hook-a hook-b] in order, default not attached", plan.Hooks)
	}
}

func testParamsMergeOrder(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	spec := keziov1alpha2.PostHookSpec{
		Params: []keziov1alpha2.PostHookParam{{Name: "greeting", Default: strPtr("from-posthook")}},
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Script:   &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .greeting }}"},
		}},
	}
	fx.mustCreatePostHook(ns, "hook", spec)
	image := fx.mustCreateImageWithParams(ns, blankDataLayout(), rawJSON(t, map[string]string{"greeting": "from-image"}))

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "hook"}},
			Params:       rawJSON(t, map[string]string{"greeting": "from-machine"}),
		},
	}
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "echo from-machine"
	if len(plan.Hooks) != 1 || len(plan.Hooks[0].Steps) != 1 || plan.Hooks[0].Steps[0].Content != want {
		t.Fatalf("rendered content = %+v, want %q (machine overrides image overrides posthook default)", plan.Hooks, want)
	}
}

func testAmbiguousDiskHints(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, []keziov1alpha2.MachineHardwareDisk{
		{DeviceName: "/dev/vda", SizeBytes: 10 << 30},
		{DeviceName: "/dev/vdb", SizeBytes: 10 << 30},
	})
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef: &keziov1alpha2.NameRef{Name: image.Name},
			// No hints: two disks are present, so "the only disk" fallback
			// is ambiguous.
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var diskErr *DiskSelectionError
	if !errors.As(err, &diskErr) {
		t.Fatalf("Build err = %v, want a *DiskSelectionError", err)
	}
}

func testUnambiguousDiskHints(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, []keziov1alpha2.MachineHardwareDisk{
		{DeviceName: "/dev/vda", SizeBytes: 10 << 30},
		{DeviceName: "/dev/vdb", SizeBytes: 20 << 30, SerialNumber: "SN-B"},
	})
	image := fx.mustCreateImage(ns, blankDataLayout())
	fx.mustCreatePostHook(ns, posthookdefaults.DefaultFinalizeHookName, posthookdefaults.Spec())

	b := &Builder{Client: fx.client, ManagerNamespace: ns}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:   &keziov1alpha2.NameRef{Name: image.Name},
			TargetDisk: &keziov1alpha2.TargetDiskHints{SerialNumber: "SN-B"},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, snap, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.TargetDisk != "/dev/vdb" {
		t.Fatalf("plan.TargetDisk = %q, want /dev/vdb", plan.TargetDisk)
	}
	if len(snap.ResolvedDisks) != 1 || snap.ResolvedDisks[0].TargetDisk != "/dev/vdb" {
		t.Fatalf("snapshot.ResolvedDisks = %+v, want [{%s /dev/vdb}]", snap.ResolvedDisks, image.Name)
	}
}

func testSlotClassification(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/nvme0n1"))

	hash := fixtureInfoHash("slot-classification")
	fx.mustCreatePartitionContentReady(ns, hash)

	layout := keziov1alpha2.ImageDiskLayout{
		PartitionTable: keziov1alpha2.PartitionTableGPT,
		SfdiskJSON:     `{"partitiontable":{}}`,
		Slots: []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleESP, ContentRef: &keziov1alpha2.NameRef{Name: store.ObjectName(hash)}},
			{Number: 2, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4"},
			{Number: 3, Role: keziov1alpha2.PartitionRoleSwap, UUID: "swap-uuid"},
		},
	}
	image := fx.mustCreateImage(ns, layout)
	subnetRef, siteIdentity := fx.mustCreateSite(ns)
	fx.mustCreateSeederPod(ns, image.Name, siteIdentity, "10.0.0.5")
	fx.mustCreatePostHook(ns, posthookdefaults.DefaultFinalizeHookName, posthookdefaults.Spec())

	b := &Builder{Client: fx.client, ManagerNamespace: ns}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec:       keziov1alpha2.MachineSpec{ImageRef: &keziov1alpha2.NameRef{Name: image.Name}, SubnetRef: subnetRef},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	plan, _, err := b.Build(context.Background(), machine, run)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Slots) != 3 {
		t.Fatalf("len(plan.Slots) = %d, want 3", len(plan.Slots))
	}
	torrent, mkfs, swap := plan.Slots[0], plan.Slots[1], plan.Slots[2]
	if torrent.Torrent == nil || torrent.Torrent.InfoHash != hash.String() || torrent.Torrent.URL != fmt.Sprintf("http://10.0.0.5:%d/torrents/%s", seederdeploy.TorrentHTTPPort, hash.String()) {
		t.Fatalf("slot 1 = %+v, want a torrent slot serving %s from 10.0.0.5", torrent, hash.String())
	}
	if mkfs.Mkfs == nil || mkfs.Mkfs.Filesystem != "ext4" {
		t.Fatalf("slot 2 = %+v, want mkfs ext4", mkfs)
	}
	if swap.Swap == nil || swap.Swap.UUID != "swap-uuid" {
		t.Fatalf("slot 3 = %+v, want swap with UUID swap-uuid", swap)
	}
	if torrent.Device != "/dev/nvme0n1p1" || mkfs.Device != "/dev/nvme0n1p2" || swap.Device != "/dev/nvme0n1p3" {
		t.Fatalf("devices = %q/%q/%q, want nvme0n1p1/p2/p3", torrent.Device, mkfs.Device, swap.Device)
	}
}

func testPartitionContentNotReady(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))

	hash := fixtureInfoHash("not-ready")
	fx.mustCreatePartitionContentPending(ns, hash)

	layout := keziov1alpha2.ImageDiskLayout{
		PartitionTable: keziov1alpha2.PartitionTableGPT,
		SfdiskJSON:     `{"partitiontable":{}}`,
		Slots: []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: store.ObjectName(hash)}},
		},
	}
	image := fx.mustCreateImage(ns, layout)

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec:       keziov1alpha2.MachineSpec{ImageRef: &keziov1alpha2.NameRef{Name: image.Name}},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Build err = %v, want a *NotReadyError", err)
	}
}

func testUnresolvedPlaceholder(t *testing.T, fx *fixtures) {
	ns := fx.mustCreateNamespace()
	fx.mustCreateMachineHardware(ns, oneDisk("/dev/vda"))
	fx.mustCreatePostHook(ns, "hook", keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Script:   &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .neverDeclared }}"},
		}},
	})
	image := fx.mustCreateImage(ns, blankDataLayout())

	b := &Builder{Client: fx.client}
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns},
		Spec: keziov1alpha2.MachineSpec{
			ImageRef:     &keziov1alpha2.NameRef{Name: image.Name},
			PostHookRefs: []keziov1alpha2.NameRef{{Name: "hook"}},
		},
	}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", UID: types.UID("uid1")}}

	_, _, err := b.Build(context.Background(), machine, run)
	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("Build err = %v, want a *ValidationError", err)
	}
}

// --- fixtures ---

func (fx *fixtures) mustCreateNamespace() string {
	fx.t.Helper()
	name := "pb-" + rand.String(8)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := fx.client.Create(context.Background(), ns); err != nil {
		fx.t.Fatalf("create namespace %s: %v", name, err)
	}
	return name
}

// testMachineName is the Machine name every subtest's fixtures use: it
// must match the machine.Name each test builds, since MachineHardware is
// looked up name-aligned with its Machine.
const testMachineName = "m1"

func (fx *fixtures) mustCreateMachineHardware(ns string, disks []keziov1alpha2.MachineHardwareDisk) {
	fx.mustCreateMachineHardwareNamed(ns, testMachineName, disks)
}

func (fx *fixtures) mustCreateMachineHardwareNamed(ns, name string, disks []keziov1alpha2.MachineHardwareDisk) {
	fx.t.Helper()
	hw := &keziov1alpha2.MachineHardware{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       keziov1alpha2.MachineHardwareSpec{Disks: disks},
	}
	if err := fx.client.Create(context.Background(), hw); err != nil {
		fx.t.Fatalf("create machinehardware %s/%s: %v", ns, name, err)
	}
}

// testImageName is the Image name every subtest's fixtures use.
const testImageName = "img1"

func (fx *fixtures) mustCreateImage(ns string, layout keziov1alpha2.ImageDiskLayout) *keziov1alpha2.Image {
	return fx.mustCreateImageWithParams(ns, layout, nil)
}

func (fx *fixtures) mustCreateImageWithParams(ns string, layout keziov1alpha2.ImageDiskLayout, params *apiextensionsv1.JSON) *keziov1alpha2.Image {
	fx.t.Helper()
	return fx.mustCreateImageFull(ns, testImageName, layout, params, nil)
}

func (fx *fixtures) mustCreateImageWithHooks(ns string, layout keziov1alpha2.ImageDiskLayout, hookRefs []keziov1alpha2.NameRef) *keziov1alpha2.Image {
	fx.t.Helper()
	return fx.mustCreateImageFull(ns, testImageName, layout, nil, hookRefs)
}

func (fx *fixtures) mustCreateImageWithHooksNamed(ns, name string, layout keziov1alpha2.ImageDiskLayout, hookRefs []keziov1alpha2.NameRef) *keziov1alpha2.Image {
	fx.t.Helper()
	return fx.mustCreateImageFull(ns, name, layout, nil, hookRefs)
}

func (fx *fixtures) mustCreateImageFull(ns, name string, layout keziov1alpha2.ImageDiskLayout, params *apiextensionsv1.JSON, hookRefs []keziov1alpha2.NameRef) *keziov1alpha2.Image {
	fx.t.Helper()
	img := &keziov1alpha2.Image{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       keziov1alpha2.ImageSpec{Layout: layout, Params: params, PostHookRefs: hookRefs},
	}
	if err := fx.client.Create(context.Background(), img); err != nil {
		fx.t.Fatalf("create image %s/%s: %v", ns, name, err)
	}
	img.Status.State = keziov1alpha2.ImageStateReady
	if err := fx.client.Status().Update(context.Background(), img); err != nil {
		fx.t.Fatalf("update image %s/%s status: %v", ns, name, err)
	}
	return img
}

// mustCreateImagePending creates an Image whose Status.State is left at
// its zero value (never set to ImageStateReady), standing in for an image
// resolveImage's reconciler has not finished processing yet.
func (fx *fixtures) mustCreateImagePending(ns string, layout keziov1alpha2.ImageDiskLayout) *keziov1alpha2.Image {
	fx.t.Helper()
	img := &keziov1alpha2.Image{
		ObjectMeta: metav1.ObjectMeta{Name: testImageName, Namespace: ns},
		Spec:       keziov1alpha2.ImageSpec{Layout: layout},
	}
	if err := fx.client.Create(context.Background(), img); err != nil {
		fx.t.Fatalf("create image %s/%s: %v", ns, img.Name, err)
	}
	return img
}

func (fx *fixtures) mustCreateConfigMap(ns, name string, data map[string]string) {
	fx.t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	}
	if err := fx.client.Create(context.Background(), cm); err != nil {
		fx.t.Fatalf("create configmap %s/%s: %v", ns, name, err)
	}
}

func (fx *fixtures) mustCreateSecret(ns, name string, data map[string][]byte) {
	fx.t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	}
	if err := fx.client.Create(context.Background(), secret); err != nil {
		fx.t.Fatalf("create secret %s/%s: %v", ns, name, err)
	}
}

func (fx *fixtures) mustCreatePostHook(ns, name string, spec keziov1alpha2.PostHookSpec) {
	fx.t.Helper()
	fx.mustCreatePostHookWithValidity(ns, name, spec, true)
}

// mustCreatePostHookPending creates a PostHook without setting its Valid
// condition, standing in for one the PostHookReconciler has not finished
// validating yet.
func (fx *fixtures) mustCreatePostHookPending(ns, name string, spec keziov1alpha2.PostHookSpec) {
	fx.t.Helper()
	fx.mustCreatePostHookWithValidity(ns, name, spec, false)
}

func (fx *fixtures) mustCreatePostHookWithValidity(ns, name string, spec keziov1alpha2.PostHookSpec, valid bool) {
	fx.t.Helper()
	ph := &keziov1alpha2.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
	if err := fx.client.Create(context.Background(), ph); err != nil {
		fx.t.Fatalf("create posthook %s/%s: %v", ns, name, err)
	}
	if !valid {
		return
	}
	meta.SetStatusCondition(&ph.Status.Conditions, metav1.Condition{
		Type: keziov1alpha2.PostHookConditionValid, Status: metav1.ConditionTrue, Reason: "TestFixture", Message: "fixture",
	})
	if err := fx.client.Status().Update(context.Background(), ph); err != nil {
		fx.t.Fatalf("update posthook %s/%s status: %v", ns, name, err)
	}
}

func (fx *fixtures) mustCreatePartitionContentReady(ns string, hash store.InfoHash) {
	fx.mustCreatePartitionContent(ns, hash, true)
}

func (fx *fixtures) mustCreatePartitionContentPending(ns string, hash store.InfoHash) {
	fx.mustCreatePartitionContent(ns, hash, false)
}

func (fx *fixtures) mustCreatePartitionContent(ns string, hash store.InfoHash, ready bool) {
	fx.t.Helper()
	pc := &keziov1alpha2.PartitionContent{
		ObjectMeta: metav1.ObjectMeta{Name: store.ObjectName(hash), Namespace: ns},
		Spec: keziov1alpha2.PartitionContentSpec{
			FSType: "ext4", UsedBytes: 1, SizeBytes: 1, LastExtentEnd: 1, PieceLength: store.PieceSize,
			Source: keziov1alpha2.PartitionContentSource{ImageName: "src", PartitionNumber: 1},
		},
	}
	if err := fx.client.Create(context.Background(), pc); err != nil {
		fx.t.Fatalf("create partitioncontent %s/%s: %v", ns, pc.Name, err)
	}
	if ready {
		pc.Status.State = keziov1alpha2.PartitionContentStateReady
		meta.SetStatusCondition(&pc.Status.Conditions, metav1.Condition{
			Type: keziov1alpha2.PartitionContentConditionReady, Status: metav1.ConditionTrue, Reason: "TestFixture", Message: "fixture",
		})
		if err := fx.client.Status().Update(context.Background(), pc); err != nil {
			fx.t.Fatalf("update partitioncontent %s/%s status: %v", ns, pc.Name, err)
		}
	}
}

// mustCreateSite creates a Subnet (declaring a seederNetworkRef so the
// CRD schema's "declares a plane" rule is satisfied, though no real NAD
// backs it in this suite) and a Site whose seederSubnetRef names it, and
// returns the NameRef a Machine's spec.subnetRef needs to resolve through
// this chain (sitederive.Resolve), plus the resulting Site identity
// string (sitederive.SiteIdentity's format) seederdeploy.Name needs.
func (fx *fixtures) mustCreateSite(ns string) (keziov1alpha2.NameRef, string) {
	fx.t.Helper()
	subnet := &keziov1alpha2.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "seeding-subnet", Namespace: ns},
		Spec: keziov1alpha2.SubnetSpec{
			SiteRef:          keziov1alpha2.NameRef{Name: "site1"},
			CIDR:             "198.51.100.0/24",
			SeederNetworkRef: &keziov1alpha2.NameRef{Name: "seeder-nad"},
		},
	}
	if err := fx.client.Create(context.Background(), subnet); err != nil {
		fx.t.Fatalf("create subnet %s/%s: %v", ns, subnet.Name, err)
	}
	site := &keziov1alpha2.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site1", Namespace: ns},
		Spec: keziov1alpha2.SiteSpec{
			SeederSubnetRef: &keziov1alpha2.NameRef{Name: subnet.Name},
		},
	}
	if err := fx.client.Create(context.Background(), site); err != nil {
		fx.t.Fatalf("create site %s/%s: %v", ns, site.Name, err)
	}
	return keziov1alpha2.NameRef{Name: subnet.Name}, ns + "/" + site.Name
}

// mustCreateSeederPod creates the per-(Image, Site) seeder Deployment
// (matching seederdeploy.Name's naming, the identity
// Builder.resolveTorrentURL looks up by) plus one matching Pod already
// carrying podIP, standing in for a scheduled, running seeder pod -
// envtest runs no kubelet to ever assign one itself.
func (fx *fixtures) mustCreateSeederPod(ns, imageName, siteIdentity, podIP string) {
	fx.t.Helper()
	pod := fx.mustCreateSeederDeploymentAndPod(ns, imageName, siteIdentity)
	pod.Status.PodIP = podIP
	if err := fx.client.Status().Update(context.Background(), pod); err != nil {
		fx.t.Fatalf("update seeder pod %s/%s status: %v", ns, pod.Name, err)
	}
}

// mustCreateSeederPodNoIP creates the seeder Deployment plus a matching
// Pod that has not reported a PodIP yet, standing in for a pod still
// pending scheduling.
func (fx *fixtures) mustCreateSeederPodNoIP(ns, imageName, siteIdentity string) {
	fx.t.Helper()
	fx.mustCreateSeederDeploymentAndPod(ns, imageName, siteIdentity)
}

// mustCreateSeederDeploymentAndPod creates the per-(Image, Site) seeder
// Deployment (matching seederdeploy.Name's naming, the identity
// Builder.resolveTorrentURL looks up by) plus one matching Pod, with no
// PodIP set - envtest runs no kubelet to ever assign one itself. Callers
// set Status.PodIP themselves when a ready pod is wanted.
func (fx *fixtures) mustCreateSeederDeploymentAndPod(ns, imageName, siteIdentity string) *corev1.Pod {
	fx.t.Helper()
	name := seederdeploy.Name(imageName, siteIdentity)
	labels := map[string]string{"app": "kezio-seeder", "instance": name}
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "ezio", Image: "example/ezio:test"}},
				},
			},
		},
	}
	if err := fx.client.Create(context.Background(), dep); err != nil {
		fx.t.Fatalf("create seeder deployment %s/%s: %v", ns, dep.Name, err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "ezio", Image: "example/ezio:test"}},
		},
	}
	if err := fx.client.Create(context.Background(), pod); err != nil {
		fx.t.Fatalf("create seeder pod %s/%s: %v", ns, pod.Name, err)
	}
	return pod
}

func oneDisk(deviceName string) []keziov1alpha2.MachineHardwareDisk {
	return []keziov1alpha2.MachineHardwareDisk{{DeviceName: deviceName, SizeBytes: 32 << 30}}
}

// blankDataLayout carries an ESP slot (number 1) ahead of its blank data
// slot (number 2) so a subtest attaching the shipped default PostHook
// (mkswap, efibootmgr) has an ESP for efibootmgr's derived "part" default
// to resolve against.
func blankDataLayout() keziov1alpha2.ImageDiskLayout {
	return keziov1alpha2.ImageDiskLayout{
		PartitionTable: keziov1alpha2.PartitionTableGPT,
		SfdiskJSON:     `{"partitiontable":{}}`,
		Slots: []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleESP, FSType: "vfat"},
			{Number: 2, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4"},
		},
	}
}

func scriptHookSpec(tag string) keziov1alpha2.PostHookSpec {
	return keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{{
			OSFamily: keziov1alpha2.OSFamilyLinux,
			Script:   &keziov1alpha2.PostHookScriptSource{Script: "echo " + tag},
		}},
	}
}

func rawJSON(t *testing.T, v map[string]string) *apiextensionsv1.JSON {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return &apiextensionsv1.JSON{Raw: raw}
}

func strPtr(s string) *string { return &s }

// fixtureInfoHash derives a deterministic, valid-looking InfoHash for
// seed's own bytes, so each subtest gets its own PartitionContent name
// without needing a real captured partition.
func fixtureInfoHash(seed string) store.InfoHash {
	sum := sha1.Sum([]byte(seed)) //nolint:gosec // fixture identity only
	hash, err := store.ParseInfoHash(hex.EncodeToString(sum[:]))
	if err != nil {
		panic(err)
	}
	return hash
}

// firstEnvtestBinaryDir mirrors internal/posthookdefaults's copy of the
// same helper: it locates the envtest binaries `make setup-envtest`
// downloads, for runs (for example from an IDE) that do not go through
// the Makefile's KUBEBUILDER_ASSETS export.
func firstEnvtestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
