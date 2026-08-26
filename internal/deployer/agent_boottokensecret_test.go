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

package deployer

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/bootserver"
)

// newAgentTestClientWithWatch builds a fake client exactly like
// newAgentTestClient, typed as client.WithWatch - interceptor.NewClient's
// required shape - so a test can wrap it to observe call ordering.
func newAgentTestClientWithWatch(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	scheme := apimachineryruntime.NewScheme()
	if err := keziov1alpha3.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1) error = %v", err)
	}
	hasSubnet := false
	for _, o := range objs {
		if _, ok := o.(*keziov1alpha3.Subnet); ok {
			hasSubnet = true
			break
		}
	}
	if !hasSubnet {
		objs = append(objs, agentTestProxySubnet())
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keziov1alpha3.DeployRun{}, &keziov1alpha3.Machine{}, &keziov1alpha3.Subnet{}).
		WithObjects(objs...).
		Build()
}

// TestAgentDeployerInspectFirstPassWritesBootTokenSecret pins the Secret
// half of issuing a boot token: the per-Machine Secret must carry the
// same plaintext TokenStore.Issue minted, the MAC it was minted for, its
// expiry, and a controller owner reference to the Machine so it is
// garbage-collected with it.
func TestAgentDeployerInspectFirstPassWritesBootTokenSecret(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	tokens := bootserver.NewTokenStore()
	d := &AgentDeployer{Client: c, Tokens: tokens}

	if _, err := d.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}

	mac, ok := bootserver.NormalizeMAC(machine.Spec.BootMACAddress)
	if !ok {
		t.Fatalf("test machine's boot MAC does not normalize: %q", machine.Spec.BootMACAddress)
	}
	wantToken, ok := tokens.Lookup(mac, machine.Status.NetBoot.TokenHash)
	if !ok {
		t.Fatalf("TokenStore has no plaintext token matching the persisted hash")
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: machine.Namespace, Name: bootserver.BootTokenSecretName(machine.Name)}
	if err := c.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("Get boot token secret: %v", err)
	}

	if secret.Type != corev1.SecretTypeOpaque {
		t.Errorf("Secret.Type = %v, want Opaque", secret.Type)
	}
	if got := string(secret.Data[bootserver.BootTokenSecretKeyToken]); got != wantToken {
		t.Errorf("Secret token = %q, want %q", got, wantToken)
	}
	if got := string(secret.Data[bootserver.BootTokenSecretKeyMAC]); got != mac {
		t.Errorf("Secret mac = %q, want %q", got, mac)
	}
	gotExpiresAt, err := time.Parse(time.RFC3339, string(secret.Data[bootserver.BootTokenSecretKeyExpiresAt]))
	if err != nil {
		t.Fatalf("Secret expiresAt does not parse as RFC3339: %v", err)
	}
	if !gotExpiresAt.Equal(machine.Status.NetBoot.ExpiresAt.Time) {
		t.Errorf("Secret expiresAt = %v, want %v", gotExpiresAt, machine.Status.NetBoot.ExpiresAt.Time)
	}

	if len(secret.OwnerReferences) != 1 {
		t.Fatalf("Secret.OwnerReferences = %+v, want exactly one", secret.OwnerReferences)
	}
	owner := secret.OwnerReferences[0]
	if owner.Kind != "Machine" || owner.Name != machine.Name || owner.UID != machine.UID {
		t.Errorf("Secret owner reference = %+v, want a Machine reference to %q (uid %q)", owner, machine.Name, machine.UID)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Errorf("Secret owner reference Controller = %v, want true", owner.Controller)
	}
}

// TestAgentDeployerInspectFirstPassWritesBootTokenSecretBeforeBMCPower
// pins the ordering commit 13cde16 established: the boot token Secret
// must be created before any BMC power call, so a manager restart
// between the two never leaves a machine powered on for a boot whose
// token cannot be recovered. The fake client and fake BMC both run
// synchronously in-process, so observing the BMC's own call counters
// from inside the Secret Create interceptor directly proves which
// happened first.
func TestAgentDeployerInspectFirstPassWritesBootTokenSecretBeforeBMCPower(t *testing.T) {
	machine := agentTestMachine(t)
	secretName := bootserver.BootTokenSecretName(machine.Name)

	var secretCreated bool
	funcs := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if secret, ok := obj.(*corev1.Secret); ok && secret.Name == secretName {
				secretCreated = true
				_, powerOn, _, _, powerCycle, _ := fakeBMCForAddress(machine.Spec.BMC.Address).calls()
				if powerOn != 0 || powerCycle != 0 {
					t.Errorf("boot token secret created after a BMC power call: powerOn=%d powerCycle=%d, want 0/0", powerOn, powerCycle)
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	c := interceptor.NewClient(newAgentTestClientWithWatch(t, machine, agentTestBMCSecret()), funcs)
	tokens := bootserver.NewTokenStore()
	d := &AgentDeployer{Client: c, Tokens: tokens}

	if _, err := d.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !secretCreated {
		t.Fatal("boot token secret was never created")
	}

	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	setPXE, powerOn, _, _, powerCycle, _ := f.calls()
	if setPXE != 1 || powerOn != 1 || powerCycle != 0 {
		t.Fatalf("BMC calls = setPXE:%d powerOn:%d powerCycle:%d, want 1/1/0", setPXE, powerOn, powerCycle)
	}
}

// TestAgentDeployerIssueBootTokenOverwritesSecretOnReArm pins that
// arming a second boot for the same Machine overwrites its boot token
// Secret in place rather than leaving the previous token's Secret
// content behind - the Secret mirrors TokenStore.Issue's own "exactly one
// token outstanding per MAC" semantics.
func TestAgentDeployerIssueBootTokenOverwritesSecretOnReArm(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	tokens := bootserver.NewTokenStore()
	d := &AgentDeployer{Client: c, Tokens: tokens}

	if _, err := d.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	firstHash := machine.Status.NetBoot.TokenHash

	// restartOnFailure forces a fresh arm exactly as a re-inspect would.
	if _, err := d.Inspect(context.Background(), machine, true); err != nil {
		t.Fatalf("second Inspect() error = %v", err)
	}
	secondHash := machine.Status.NetBoot.TokenHash
	if secondHash == firstHash {
		t.Fatalf("second arm did not mint a fresh token hash")
	}

	mac, _ := bootserver.NormalizeMAC(machine.Spec.BootMACAddress)
	wantToken, ok := tokens.Lookup(mac, secondHash)
	if !ok {
		t.Fatalf("TokenStore has no plaintext token matching the second arm's hash")
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: machine.Namespace, Name: bootserver.BootTokenSecretName(machine.Name)}
	if err := c.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("Get boot token secret: %v", err)
	}
	if got := string(secret.Data[bootserver.BootTokenSecretKeyToken]); got != wantToken {
		t.Errorf("Secret token after re-arm = %q, want the second arm's token %q", got, wantToken)
	}
}
