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
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/agentserver"
	"github.com/tjjh89017/kezio/internal/bootserver"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/store"
)

// fakeDaemon stands in for one ezio daemon's torrent set, shared by every
// fakeSeederClient dialed against the same target: AddTorrent adds
// durably, GetTorrentStatus reflects everything added so far.
type fakeDaemon struct {
	mu       sync.Mutex
	torrents map[string]seeder.Torrent
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{torrents: make(map[string]seeder.Torrent)}
}

type fakeSeederClient struct{ d *fakeDaemon }

func (c fakeSeederClient) AddTorrent(_ context.Context, _ []byte, savePath string, _ bool, _, _ int32) error {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	// The content hash is the save_path's leaf directory name.
	hash := filepath.Base(savePath)
	c.d.torrents[hash] = seeder.Torrent{Hash: hash}
	return nil
}

func (c fakeSeederClient) PauseTorrent(_ context.Context, hash string) error {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	t := c.d.torrents[hash]
	t.IsPaused = true
	c.d.torrents[hash] = t
	return nil
}

func (c fakeSeederClient) ResumeTorrent(_ context.Context, hash string) error {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	t := c.d.torrents[hash]
	t.IsPaused = false
	c.d.torrents[hash] = t
	return nil
}

func (c fakeSeederClient) GetTorrentStatus(_ context.Context, _ []string) (map[string]seeder.Torrent, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	out := make(map[string]seeder.Torrent, len(c.d.torrents))
	maps.Copy(out, c.d.torrents)
	return out, nil
}

func (c fakeSeederClient) Close() error { return nil }

// fakeSeederRegistry hands out (and remembers) one *fakeDaemon per dial
// target, so a test can dial the "same daemon" repeatedly.
type fakeSeederRegistry struct {
	mu      sync.Mutex
	daemons map[string]*fakeDaemon
}

func newFakeSeederRegistry() *fakeSeederRegistry {
	return &fakeSeederRegistry{daemons: make(map[string]*fakeDaemon)}
}

func (r *fakeSeederRegistry) dial(target string) (SeederEZIOClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.daemons[target]
	if !ok {
		d = newFakeDaemon()
		r.daemons[target] = d
	}
	return fakeSeederClient{d: d}, nil
}

// writeFixtureContent writes a minimal, valid content directory (a
// torrent.info plus its one extent file under content/, per
// store.ValidateContentDir) under a fresh temp store root, and returns
// the root and the content's info hash.
func writeFixtureContent() (root string, hash store.InfoHash) {
	root, err := os.MkdirTemp("", "seeder-store-*")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = os.RemoveAll(root) })

	info := &store.TorrentInfo{
		BlockSize:   4096,
		BlocksTotal: 100,
		Extents:     []store.Extent{{Offset: 0, Length: store.PieceSize}},
		PieceHashes: []store.PieceHash{{1, 2, 3, 4, 5}},
	}
	hash, err = store.ComputeInfoHash(info)
	Expect(err).NotTo(HaveOccurred())

	dir := store.ContentDir(root, hash)
	Expect(os.MkdirAll(store.ContentDataDir(dir), 0o755)).To(Succeed())

	infoFile, err := os.Create(store.ContentTorrentInfoPath(root, hash)) //nolint:gosec // test fixture path
	Expect(err).NotTo(HaveOccurred())
	Expect(store.WriteTorrentInfo(infoFile, info)).To(Succeed())
	Expect(infoFile.Close()).To(Succeed())

	extentPath := store.ContentExtentPath(root, hash, 0)
	Expect(os.WriteFile(extentPath, make([]byte, store.PieceSize), 0o644)).To(Succeed()) //nolint:gosec // test fixture path

	return root, hash
}

// createReadyImage creates an Image that is immediately Ready with one
// partition carrying hash, bypassing the ingest state machine.
func createReadyImage(ctx context.Context, hash store.InfoHash) *keziov1alpha1.Image {
	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("img-%s", rand.String(5)),
			Namespace: "default",
		},
		Spec: keziov1alpha1.ImageSpec{
			Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatRaw},
		},
	}
	Expect(k8sClient.Create(ctx, image)).To(Succeed())

	image.Status.State = keziov1alpha1.ImageStateReady
	image.Status.Partitions = []keziov1alpha1.ImagePartitionStatus{
		{Role: "data", InfoHash: hash.String()},
	}
	Expect(k8sClient.Status().Update(ctx, image)).To(Succeed())
	return image
}

// seederDeploymentTestGracePeriod is a fixed, arbitrary grace period the
// tests below advance a fake clock past or stop short of; its value only
// needs to be distinct from zero and from the elapsed offsets the tests
// use.
const seederDeploymentTestGracePeriod = 5 * time.Minute

// newSeederDeploymentReconciler returns an ImageReconciler with
// SeederDeploymentConfig enabled and a fake clock the test controls via
// the returned *time.Time, so grace-period countdowns are exercised
// without sleeping.
func newSeederDeploymentReconciler() (*ImageReconciler, *time.Time) {
	now := time.Now()
	clock := &now
	r := &ImageReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		SeederDeployment: SeederDeploymentConfig{
			Image:       "ezio-seeder:test",
			GracePeriod: seederDeploymentTestGracePeriod,
			Now:         func() time.Time { return *clock },
		},
	}
	return r, clock
}

// seederTestSites remembers which (namespace, site) pairs
// ensureSeederTestSite has already provisioned, so repeated calls reuse
// the same fixtures instead of failing on AlreadyExists.
var (
	seederTestSitesMu sync.Mutex
	seederTestSites   = map[string]bool{}
)

// ensureSeederTestSite creates, once per (namespace, site), a Subnet and
// a Site both named site, with the Subnet's spec.siteRef pointing at the
// Site and the Site's spec.seederSubnetRef pointing back at the same
// Subnet - one Subnet doubling as both the Machine's own and the Site's
// seeder Subnet (an explicitly supported sharing).
func ensureSeederTestSite(ctx context.Context, namespace, site string) {
	seederTestSitesMu.Lock()
	defer seederTestSitesMu.Unlock()
	key := namespace + "/" + site
	if seederTestSites[key] {
		return
	}

	subnet := &keziov1alpha1.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: site, Namespace: namespace},
		Spec: keziov1alpha1.SubnetSpec{
			SiteRef:         keziov1alpha1.NameRef{Name: site},
			CIDR:            "192.0.2.0/24",
			BootdServerIP:   "192.0.2.2",
			BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
			DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, subnet)).To(Succeed())

	siteObj := &keziov1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: site, Namespace: namespace},
		Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: site}},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, siteObj)).To(Succeed())

	seederTestSites[key] = true
}

// createSeederTestMachine creates a Machine in state referencing image
// at site, bypassing MachineReconciler. It provisions site's Subnet via
// ensureSeederTestSite first; callers needing a Machine on one of
// several Subnets sharing a Site use createSeederTestMachineOnSubnet
// directly.
func createSeederTestMachine(ctx context.Context, namespace, image, site, state string, conds ...metav1.Condition) *keziov1alpha1.Machine {
	ensureSeederTestSite(ctx, namespace, site)
	return createSeederTestMachineOnSubnet(ctx, namespace, image, site, state, conds...)
}

// createSeederTestMachineOnSubnet creates a Machine in state, referencing
// image, with spec.subnetRef naming subnet directly, so the caller
// controls which Subnet (and Site) the Machine resolves to. Each
// condition gets a LastTransitionTime if missing (the API server
// requires one).
func createSeederTestMachineOnSubnet(ctx context.Context, namespace, image, subnet, state string, conds ...metav1.Condition) *keziov1alpha1.Machine {
	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("m-%s", rand.String(5)),
			Namespace: namespace,
		},
		Spec: keziov1alpha1.MachineSpec{
			BMC: keziov1alpha1.MachineBMC{
				Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha1.SecretReference{Name: "seeder-test-bmc"},
			},
			BootMACAddress: fmt.Sprintf("aa:bb:cc:dd:ee:%02x", rand.Intn(250)+1),
			ImageRef:       &keziov1alpha1.NameRef{Name: image},
			SubnetRef:      keziov1alpha1.NameRef{Name: subnet},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, machine)).To(Succeed())

	for i := range conds {
		if conds[i].LastTransitionTime.IsZero() {
			conds[i].LastTransitionTime = metav1.Now()
		}
	}
	machine.Status.State = state
	machine.Status.Conditions = conds
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, machine)).To(Succeed())
	return machine
}

// createSiteWithTwoSubnets creates a fresh Site (random name, so
// concurrent Its don't collide) with two Subnets naming it as
// spec.siteRef, and the Site's seederSubnetRef pointing at the second.
// Returns the Site's name and both Subnets' names, machineSubnet before
// seederSubnet.
func createSiteWithTwoSubnets(ctx context.Context, namespace string) (site, machineSubnet, seederSubnet string) {
	site = fmt.Sprintf("site-%s", rand.String(5))
	machineSubnet = site + "-machine"
	seederSubnet = site + "-seeder"

	subA := &keziov1alpha1.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: machineSubnet, Namespace: namespace},
		Spec: keziov1alpha1.SubnetSpec{
			SiteRef:         keziov1alpha1.NameRef{Name: site},
			CIDR:            "192.0.2.0/24",
			BootdServerIP:   "192.0.2.2",
			BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
			DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, subA)).To(Succeed())

	subB := &keziov1alpha1.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: seederSubnet, Namespace: namespace},
		Spec: keziov1alpha1.SubnetSpec{
			SiteRef:         keziov1alpha1.NameRef{Name: site},
			CIDR:            "192.0.3.0/24",
			BootdServerIP:   "192.0.3.2",
			BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
			DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, subB)).To(Succeed())

	siteObj := &keziov1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: site, Namespace: namespace},
		Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: seederSubnet}},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, siteObj)).To(Succeed())

	return site, machineSubnet, seederSubnet
}

// reconcileImage calls Reconcile twice: the first call on a freshly
// created Image only attaches the finalizer and returns early, so a
// single call would never reach reconcileSeederDeployments.
func reconcileImage(ctx context.Context, r *ImageReconciler, key types.NamespacedName) (reconcile.Result, error) {
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		return reconcile.Result{}, err
	}
	return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
}

// seederDeploymentsForImage lists every Deployment owned by image (via
// seederDeploymentImageLabel), keyed by the site annotation
// reconcileSeederDeployments stamps.
func seederDeploymentsForImage(ctx context.Context, image *keziov1alpha1.Image) map[string]appsv1.Deployment {
	var deployments appsv1.DeploymentList
	ExpectWithOffset(1, k8sClient.List(ctx, &deployments,
		client.InNamespace(image.Namespace),
		client.MatchingLabels{SeederDeploymentImageLabel: image.Name},
	)).To(Succeed())

	bySite := make(map[string]appsv1.Deployment, len(deployments.Items))
	for _, dep := range deployments.Items {
		bySite[dep.Annotations[SeederDeploymentSiteAnnotation]] = dep
	}
	ExpectWithOffset(1, bySite).To(HaveLen(len(deployments.Items)),
		"two Deployments carry the identical Site annotation value - a duplicate object (stray or create-race), not a per-Subnet regression")
	return bySite
}

// cleanupSeederTest deletes every given Machine, deletes any seeder
// Deployment left for image (envtest runs no garbage collector, so an
// owner reference alone never reaps one here), and drains image's
// finalizer - otherwise a Ready Image with the fixture's fixed content
// hash would leak into every other spec in this shared suite.
func cleanupSeederTest(ctx context.Context, r *ImageReconciler, image *keziov1alpha1.Image, machines ...*keziov1alpha1.Machine) {
	for _, m := range machines {
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, m))).To(Succeed())
	}
	for _, dep := range seederDeploymentsForImage(ctx, image) {
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, &dep))).To(Succeed())
	}
	deleteImageAndFinalize(ctx, r, types.NamespacedName{Name: image.Name, Namespace: image.Namespace}, image)
}

var _ = Describe("Seeder Deployment lifecycle", func() {
	ctx := context.Background()

	It("does nothing when SeederDeployment is not configured", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image) })

		result, err := reconcileImage(ctx, r, types.NamespacedName{Name: image.Name, Namespace: image.Namespace})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(BeEmpty())
	})

	It("creates a Deployment, owned by the Image, when a Machine enters Provisioning", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey("default/site-a"))
		dep := bySite["default/site-a"]
		Expect(dep.OwnerReferences).To(HaveLen(1))
		Expect(dep.OwnerReferences[0].Name).To(Equal(image.Name))
		Expect(dep.OwnerReferences[0].Kind).To(Equal("Image"))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: "default/site-a", MachineCount: 1},
		))
	})

	It("issues no write on a quiescent reconcile - the Deployment's and the Image's status resourceVersion are unchanged by a second reconcile with nothing changed", func() {
		// resourceVersion is server-assigned and changes on every write, so
		// comparing it is what proves no write happened.
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		// envtest runs no controller-manager, so AvailableReplicas is
		// always 0: this first reconcile legitimately stamps
		// seeder-unready-since (a real transition), so only the next one
		// is truly quiescent.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		depBefore := seederDeploymentsForImage(ctx, image)["default/site-a"]
		Expect(depBefore.Annotations).To(HaveKey(seederDeploymentUnreadySinceAnnotation))

		var imageBefore keziov1alpha1.Image
		Expect(k8sClient.Get(ctx, key, &imageBefore)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		depAfter := seederDeploymentsForImage(ctx, image)["default/site-a"]
		Expect(depAfter.ResourceVersion).To(Equal(depBefore.ResourceVersion), "a quiescent reconcile must issue no Deployment Update - resourceVersion must not move")
		Expect(depAfter.Generation).To(Equal(depBefore.Generation))

		var imageAfter keziov1alpha1.Image
		Expect(k8sClient.Get(ctx, key, &imageAfter)).To(Succeed())
		Expect(imageAfter.ResourceVersion).To(Equal(imageBefore.ResourceVersion), "a quiescent reconcile must issue no Image status Update either")
	})

	It("counts every Provisioning Machine referencing the Image at one site", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		m1 := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		m2 := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, m1, m2) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveLen(1))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: "default/site-a", MachineCount: 2},
		))
	})

	It("collapses two Machines on two distinct Subnets of the same Site into a single seeder Deployment", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		site, machineSubnet, seederSubnet := createSiteWithTwoSubnets(ctx, image.Namespace)
		m1 := createSeederTestMachineOnSubnet(ctx, image.Namespace, image.Name, machineSubnet, keziov1alpha1.MachineStateProvisioning)
		m2 := createSeederTestMachineOnSubnet(ctx, image.Namespace, image.Name, seederSubnet, keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, m1, m2) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		siteIdentity := image.Namespace + "/" + site

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveLen(1), "two Subnets in one Site must produce exactly one seeder Deployment, not one per Subnet")
		Expect(bySite).To(HaveKey(siteIdentity))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: siteIdentity, MachineCount: 2},
		))
	})

	It("creates one Deployment per site for the same Image", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		mA := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		mB := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-b", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, mA, mB) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveLen(2))
		Expect(bySite).To(HaveKey("default/site-a"))
		Expect(bySite).To(HaveKey("default/site-b"))
		Expect(bySite["default/site-a"].Name).NotTo(Equal(bySite["default/site-b"].Name))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: "default/site-a", MachineCount: 1},
			keziov1alpha1.ImageSeederSiteStatus{Site: "default/site-b", MachineCount: 1},
		))
	})

	It("keeps a Machine in error backoff retrying a provisioning failure holding its reference", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateError,
			metav1.Condition{Type: keziov1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reasonProvisionFailed})
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey("default/site-a"))
	})

	It("does not create a Deployment for a Machine in error backoff retrying a register failure", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateError,
			metav1.Condition{Type: keziov1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reasonRegisterFailed})
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(seederDeploymentsForImage(ctx, image)).To(BeEmpty())
	})

	It("keeps the Deployment through the grace period after demand drops, then deletes it once the grace period elapses", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, clock := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(seederDeploymentsForImage(ctx, image)).To(HaveKey("default/site-a"))

		By("the Machine finishing (Provisioned) - demand drops to zero")
		machine.Status.State = keziov1alpha1.MachineStateProvisioned
		Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		By("the Deployment surviving immediately after demand drops (grace period, not immediate teardown)")
		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey("default/site-a"))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: "default/site-a", MachineCount: 0},
		))

		By("reconciling again before the grace period elapses - still present")
		*clock = clock.Add(seederDeploymentTestGracePeriod / 2)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(seederDeploymentsForImage(ctx, image)).To(HaveKey("default/site-a"))

		By("reconciling once the grace period has elapsed - the Deployment is deleted")
		*clock = clock.Add(seederDeploymentTestGracePeriod)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(seederDeploymentsForImage(ctx, image)).NotTo(HaveKey("default/site-a"))

		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(BeEmpty())
	})

	It("deletes the Deployment exactly when the clock reaches the grace-period deadline", func() {
		// remaining == 0 must resolve to "delete", not "requeue" -
		// off-by-one to remaining >= 0 would leave this Deployment stuck
		// forever.
		image := createReadyImage(ctx, mustFixtureHash())
		r, clock := newSeederDeploymentReconciler()
		// emptySince round-trips through an RFC3339 (second-precision)
		// annotation, so a sub-second clock would drift the tie negative.
		*clock = clock.Truncate(time.Second)
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(seederDeploymentsForImage(ctx, image)).To(HaveKey("default/site-a"))

		machine.Status.State = keziov1alpha1.MachineStateProvisioned
		Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(seederDeploymentsForImage(ctx, image)).To(HaveKey("default/site-a"))

		By("reconciling at exactly emptySince + gracePeriod - the Deployment is deleted, not requeued")
		*clock = clock.Add(seederDeploymentTestGracePeriod)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(seederDeploymentsForImage(ctx, image)).NotTo(HaveKey("default/site-a"))
	})

	It("cancels the grace-period countdown when demand returns before it elapses", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, clock := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		var second *keziov1alpha1.Machine
		DeferCleanup(func() {
			machines := []*keziov1alpha1.Machine{machine}
			if second != nil {
				machines = append(machines, second)
			}
			cleanupSeederTest(ctx, r, image, machines...)
		})

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		firstDeployment := seederDeploymentsForImage(ctx, image)["default/site-a"]

		By("the Machine finishing - demand drops to zero and the grace countdown starts")
		machine.Status.State = keziov1alpha1.MachineStateProvisioned
		Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("a second Machine starting a deploy against the same Image and site before the grace period elapses")
		*clock = clock.Add(seederDeploymentTestGracePeriod / 2)
		second = createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("advancing past the original grace deadline - the Deployment survives because the countdown was cancelled")
		*clock = clock.Add(seederDeploymentTestGracePeriod)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey("default/site-a"))
		Expect(bySite["default/site-a"].Name).To(Equal(firstDeployment.Name), "the same Deployment should have been reused, not recreated")

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: "default/site-a", MachineCount: 1},
		))
	})

	It("re-resolves a Machine's seeder demand to a different Site once its Subnet's siteRef moves there", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, clock := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		siteOld := fmt.Sprintf("site-old-%s", rand.String(5))
		siteNew := fmt.Sprintf("site-new-%s", rand.String(5))
		roamingSubnet := fmt.Sprintf("roaming-%s", rand.String(5))

		// Both Sites declare the same roaming Subnet as their seeder
		// attachment point: resolveSeederSubnet only follows
		// SeederSubnetRef by name, so whichever Site the Subnet's siteRef
		// currently names is the one that gets a Deployment.
		Expect(k8sClient.Create(ctx, &keziov1alpha1.Site{
			ObjectMeta: metav1.ObjectMeta{Name: siteOld, Namespace: image.Namespace},
			Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: roamingSubnet}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &keziov1alpha1.Site{
			ObjectMeta: metav1.ObjectMeta{Name: siteNew, Namespace: image.Namespace},
			Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: roamingSubnet}},
		})).To(Succeed())

		subnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: roamingSubnet, Namespace: image.Namespace},
			Spec: keziov1alpha1.SubnetSpec{
				SiteRef:         keziov1alpha1.NameRef{Name: siteOld},
				CIDR:            "192.0.4.0/24",
				BootdServerIP:   "192.0.4.2",
				BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
				DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
			},
		}
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		machine := createSeederTestMachineOnSubnet(ctx, image.Namespace, image.Name, roamingSubnet, keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		siteOldIdentity := image.Namespace + "/" + siteOld
		siteNewIdentity := image.Namespace + "/" + siteNew

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(seederDeploymentsForImage(ctx, image)).To(HaveKey(siteOldIdentity))
		Expect(seederDeploymentsForImage(ctx, image)).NotTo(HaveKey(siteNewIdentity))

		By("moving the Subnet's siteRef to the new Site, without touching the Machine itself")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: roamingSubnet, Namespace: image.Namespace}, subnet)).To(Succeed())
		subnet.Spec.SiteRef = keziov1alpha1.NameRef{Name: siteNew}
		Expect(k8sClient.Update(ctx, subnet)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey(siteNewIdentity), "demand must follow the Subnet's new siteRef")
		Expect(bySite).To(HaveKey(siteOldIdentity), "the old Site's Deployment survives immediately - the grace period, not instant teardown")

		By("advancing past the grace period - the old Site's now-empty Deployment is deleted")
		*clock = clock.Add(seederDeploymentTestGracePeriod * 2)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		bySite = seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey(siteNewIdentity))
		Expect(bySite).NotTo(HaveKey(siteOldIdentity))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: siteNewIdentity, MachineCount: 1},
		))
	})

	It("takes the seeder Deployment's NodeSelector from the Site's seeder Subnet, not the Machine's own Subnet", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		// Two distinct Subnets in the same Site, each with its own
		// NodeSelector: using the same Subnet for both roles (as
		// ensureSeederTestSite does elsewhere) would still pass even if
		// buildSeederDeployment read the wrong Subnet's selector.
		site := "site-selector"
		machineSubnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: site + "-machine", Namespace: image.Namespace},
			Spec: keziov1alpha1.SubnetSpec{
				SiteRef:         keziov1alpha1.NameRef{Name: site},
				CIDR:            "192.0.2.0/24",
				BootdServerIP:   "192.0.2.2",
				BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
				DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
				NodeSelector:    map[string]string{"segment": "machine-vlan"},
			},
		}
		Expect(k8sClient.Create(ctx, machineSubnet)).To(Succeed())

		seederSubnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: site + "-seeder", Namespace: image.Namespace},
			Spec: keziov1alpha1.SubnetSpec{
				SiteRef:         keziov1alpha1.NameRef{Name: site},
				CIDR:            "192.0.3.0/24",
				BootdServerIP:   "192.0.3.2",
				BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
				DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
				NodeSelector:    map[string]string{"segment": "seeder-vlan"},
			},
		}
		Expect(k8sClient.Create(ctx, seederSubnet)).To(Succeed())

		siteObj := &keziov1alpha1.Site{
			ObjectMeta: metav1.ObjectMeta{Name: site, Namespace: image.Namespace},
			Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: seederSubnet.Name}},
		}
		Expect(k8sClient.Create(ctx, siteObj)).To(Succeed())

		machine := &keziov1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("m-%s", rand.String(5)), Namespace: image.Namespace},
			Spec: keziov1alpha1.MachineSpec{
				BMC: keziov1alpha1.MachineBMC{
					Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
					CredentialsSecretRef: keziov1alpha1.SecretReference{Name: "seeder-test-bmc"},
				},
				BootMACAddress: fmt.Sprintf("aa:bb:cc:dd:ee:%02x", rand.Intn(250)+1),
				ImageRef:       &keziov1alpha1.NameRef{Name: image.Name},
				SubnetRef:      keziov1alpha1.NameRef{Name: machineSubnet.Name},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		machine.Status.State = keziov1alpha1.MachineStateProvisioning
		Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		siteIdentity := image.Namespace + "/" + site

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey(siteIdentity))
		dep := bySite[siteIdentity]
		Expect(dep.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"segment": "seeder-vlan"}),
			"the seeder Deployment must take its NodeSelector from the Site's seeder Subnet, not the Machine's own Subnet")
	})

	It("updates an already-existing seeder Deployment when its seeder Subnet's NodeSelector changes", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		site := fmt.Sprintf("site-drift-%s", rand.String(5))
		seederSubnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: site + "-seeder", Namespace: image.Namespace},
			Spec: keziov1alpha1.SubnetSpec{
				SiteRef:         keziov1alpha1.NameRef{Name: site},
				CIDR:            "192.0.2.0/24",
				BootdServerIP:   "192.0.2.2",
				BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
				DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
				NodeSelector:    map[string]string{"segment": "seeder-vlan-old"},
			},
		}
		Expect(k8sClient.Create(ctx, seederSubnet)).To(Succeed())

		siteObj := &keziov1alpha1.Site{
			ObjectMeta: metav1.ObjectMeta{Name: site, Namespace: image.Namespace},
			Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: seederSubnet.Name}},
		}
		Expect(k8sClient.Create(ctx, siteObj)).To(Succeed())

		machine := createSeederTestMachineOnSubnet(ctx, image.Namespace, image.Name, seederSubnet.Name, keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		siteIdentity := image.Namespace + "/" + site

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		before := seederDeploymentsForImage(ctx, image)[siteIdentity]
		Expect(before.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"segment": "seeder-vlan-old"}))

		By("changing the seeder Subnet's NodeSelector after the Deployment already exists")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: seederSubnet.Name, Namespace: image.Namespace}, seederSubnet)).To(Succeed())
		seederSubnet.Spec.NodeSelector = map[string]string{"segment": "seeder-vlan-new"}
		Expect(k8sClient.Update(ctx, seederSubnet)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		after := seederDeploymentsForImage(ctx, image)[siteIdentity]
		Expect(after.Name).To(Equal(before.Name), "the same Deployment must be updated in place, not recreated")
		Expect(after.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"segment": "seeder-vlan-new"}),
			"an already-existing seeder Deployment must pick up a changed NodeSelector on the next reconcile")
	})

	It("leaves an already-existing seeder Deployment untouched, and marks Image status degraded, when its Site's seeder Subnet reference disappears", func() {
		// When the seeder Subnet no longer resolves, buildSeederDeployment's
		// inputs go nil - which would silently drop the pod onto the
		// default pod network and off its NodeSelector while still
		// reporting Available. The Deployment must be left exactly as it
		// is; the break must surface on Image status instead.
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		site := fmt.Sprintf("site-lost-ref-%s", rand.String(5))
		seederSubnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: site + "-seeder", Namespace: image.Namespace},
			Spec: keziov1alpha1.SubnetSpec{
				SiteRef:          keziov1alpha1.NameRef{Name: site},
				CIDR:             "192.0.2.0/24",
				BootdServerIP:    "192.0.2.2",
				BootdNetworkRef:  keziov1alpha1.NameRef{Name: "bootd-nad"},
				SeederNetworkRef: &keziov1alpha1.NameRef{Name: "seeder-nad"},
				DHCP:             keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
				NodeSelector:     map[string]string{"segment": "seeder-vlan"},
			},
		}
		Expect(k8sClient.Create(ctx, seederSubnet)).To(Succeed())

		siteObj := &keziov1alpha1.Site{
			ObjectMeta: metav1.ObjectMeta{Name: site, Namespace: image.Namespace},
			Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: seederSubnet.Name}},
		}
		Expect(k8sClient.Create(ctx, siteObj)).To(Succeed())

		machine := createSeederTestMachineOnSubnet(ctx, image.Namespace, image.Name, seederSubnet.Name, keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		siteIdentity := image.Namespace + "/" + site

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		before := seederDeploymentsForImage(ctx, image)[siteIdentity]
		Expect(before.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"segment": "seeder-vlan"}))
		Expect(before.Spec.Template.Annotations).To(HaveKey(multusDefaultNetworkAnnotation),
			"the Deployment must start with the Multus default-network annotation this whole test is about not losing")

		By("unsetting the Site's seederSubnetRef while the Machine is still provisioning against this Deployment")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: site, Namespace: image.Namespace}, siteObj)).To(Succeed())
		siteObj.Spec.SeederSubnetRef = nil
		Expect(k8sClient.Update(ctx, siteObj)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		after := seederDeploymentsForImage(ctx, image)[siteIdentity]
		Expect(after.Name).To(Equal(before.Name), "the Deployment must not be deleted or recreated")
		Expect(after.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"segment": "seeder-vlan"}),
			"an already-existing seeder Deployment must keep its NodeSelector when its Site's seeder Subnet reference disappears")
		Expect(after.Spec.Template.Annotations).To(HaveKeyWithValue(multusDefaultNetworkAnnotation, before.Spec.Template.Annotations[multusDefaultNetworkAnnotation]),
			"an already-existing seeder Deployment must keep its Multus default-network annotation when its Site's seeder Subnet reference disappears")

		updatedImage := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updatedImage)).To(Succeed())
		cond := apimeta.FindStatusCondition(updatedImage.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)
		Expect(cond).NotTo(BeNil(), "the broken seeder Subnet reference must be visible on Image status, not silent")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("SeederSubnetRefUnset"))
		Expect(cond.Message).To(ContainSubstring(siteIdentity))

		By("pointing the seederSubnetRef at a Subnet that does not exist - the same Deployment stays untouched, but the Reason now reports the missing name")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: site, Namespace: image.Namespace}, siteObj)).To(Succeed())
		siteObj.Spec.SeederSubnetRef = &keziov1alpha1.NameRef{Name: "ghost-seeder-subnet"}
		Expect(k8sClient.Update(ctx, siteObj)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred(), "a dangling seederSubnetRef must not fail the reconcile")

		dangling := seederDeploymentsForImage(ctx, image)[siteIdentity]
		Expect(dangling.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"segment": "seeder-vlan"}))
		Expect(dangling.Spec.Template.Annotations).To(HaveKeyWithValue(multusDefaultNetworkAnnotation, before.Spec.Template.Annotations[multusDefaultNetworkAnnotation]))

		Expect(k8sClient.Get(ctx, key, updatedImage)).To(Succeed())
		cond = apimeta.FindStatusCondition(updatedImage.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("SeederSubnetRefNotFound"),
			"a ref naming a Subnet that does not exist must not be reported as an unset ref - the remedy is to create that Subnet, not to set the field")
		Expect(cond.Message).To(ContainSubstring(siteIdentity))
		Expect(cond.Message).To(ContainSubstring(image.Namespace+"/ghost-seeder-subnet"),
			"the condition must name the Subnet that is missing, so the operator learns which name is wrong")

		By("restoring the Site's seederSubnetRef - the condition must clear")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: site, Namespace: image.Namespace}, siteObj)).To(Succeed())
		siteObj.Spec.SeederSubnetRef = &keziov1alpha1.NameRef{Name: seederSubnet.Name}
		Expect(k8sClient.Update(ctx, siteObj)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, updatedImage)).To(Succeed())
		Expect(apimeta.FindStatusCondition(updatedImage.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)).To(BeNil(),
			"restoring the seeder Subnet reference must clear ImageConditionSeederDegraded, not leave it stuck")
	})

	It("surfaces a seeder pod that never becomes Ready as degraded once past the grace period, and clears it once demand drops", func() {
		// envtest runs no controller-manager, so AvailableReplicas is
		// always 0 - standing in for a pod stuck unready in production
		// (crashloop, bad image, unsatisfiable NodeSelector).
		image := createReadyImage(ctx, mustFixtureHash())
		r, clock := newSeederDeploymentReconciler()
		*clock = clock.Truncate(time.Second)
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		By("the next reconcile observes zero AvailableReplicas and starts the grace-period countdown, not yet degraded")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		dep := seederDeploymentsForImage(ctx, image)["default/site-a"]
		Expect(dep.Annotations).To(HaveKey(seederDeploymentUnreadySinceAnnotation))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)).To(BeNil())

		By("advancing past the grace period - the still-unready pod is now surfaced as degraded")
		*clock = clock.Add(seederDeploymentTestGracePeriod)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		cond := apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)
		Expect(cond).NotTo(BeNil(), "a seeder pod stuck unready past the grace period must be visible on Image status, not left for agents to poll ActionWait forever")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("SeederPodUnready"))
		Expect(cond.Message).To(ContainSubstring("default/site-a"))

		By("the Machine finishing - demand drops, and the unready countdown is cleared even though the pod stayed unready")
		machine.Status.State = keziov1alpha1.MachineStateProvisioned
		Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		dep = seederDeploymentsForImage(ctx, image)["default/site-a"]
		Expect(dep.Annotations).NotTo(HaveKey(seederDeploymentUnreadySinceAnnotation))
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)).To(BeNil(),
			"demand dropping ends the unready judgment too - the Deployment is now draining, not being scored on readiness")
	})

	It("lets internal/agentserver's deploy plan find the exact seeder Deployment reconcileSeederDeployments created for the same Machine", func() {
		// seederDemandBySite (this package) and agentserver's
		// resolveSeederTorrentURL each derive a Machine's Site
		// independently via sitederive.Resolve, then key a Deployment
		// name off it - this drives both real code paths to catch drift
		// between them.
		layout := &keziov1alpha1.ImageLayout{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("layout-%s", rand.String(5)), Namespace: "default"},
			Spec:       keziov1alpha1.ImageLayoutSpec{SfdiskJSON: `{"partitiontable":{"label":"gpt"}}`},
		}
		Expect(k8sClient.Create(ctx, layout)).To(Succeed())

		image := &keziov1alpha1.Image{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("img-%s", rand.String(5)), Namespace: "default"},
			Spec:       keziov1alpha1.ImageSpec{Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatRaw}},
		}
		Expect(k8sClient.Create(ctx, image)).To(Succeed())
		image.Status.State = keziov1alpha1.ImageStateReady
		image.Status.Disk = &keziov1alpha1.ImageDiskStatus{
			PartitionTable: keziov1alpha1.PartitionTableGPT,
			LayoutRef:      &keziov1alpha1.NameRef{Name: layout.Name},
		}
		image.Status.Partitions = []keziov1alpha1.ImagePartitionStatus{
			{Number: 1, Role: keziov1alpha1.PartitionRoleData, FSType: "ext4", InfoHash: mustFixtureHash().String()},
		}
		Expect(k8sClient.Status().Update(ctx, image)).To(Succeed())

		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		_, machineSubnet, _ := createSiteWithTwoSubnets(ctx, image.Namespace)
		machine := createSeederTestMachineOnSubnet(ctx, image.Namespace, image.Name, machineSubnet, keziov1alpha1.MachineStateProvisioning)
		machine.Status.Provisioning = &keziov1alpha1.MachineProvisioningStatus{
			Image: &keziov1alpha1.MachineProvisionedImage{
				ImageRef:   keziov1alpha1.NameRef{Name: image.Name},
				TargetDisk: "/dev/nvme0n1",
			},
		}
		token := "convergence-test-token-" + rand.String(5)
		machine.Status.AgentSession = &keziov1alpha1.MachineAgentSessionStatus{
			TokenHash: bootserver.HashToken(token),
			ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)),
		}
		Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())
		DeferCleanup(func() {
			cleanupSeederTest(ctx, r, image, machine)
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, layout))).To(Succeed())
		})

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveLen(1))
		Expect(bySite).NotTo(HaveKey(machine.Spec.SubnetRef.Name), "the Deployment must be keyed by the Machine's Site, not its own Subnet name")
		var dep appsv1.Deployment
		for _, d := range bySite {
			dep = d
		}
		createSeederDeploymentPod(ctx, &dep, "10.9.9.20")

		agentSrv := agentserver.New(k8sClient, agentserver.Config{})
		req := httptest.NewRequest(http.MethodGet, agentapi.NextPathPrefix+machine.Name+agentapi.NextPathSuffix, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(agentapi.AgentSchemaVersionHeader, strconv.Itoa(agentapi.PlanSchemaVersion))
		rec := httptest.NewRecorder()
		agentSrv.Handler().ServeHTTP(rec, req)

		Expect(rec.Code).To(Equal(http.StatusOK))
		var resp agentapi.NextResponse
		Expect(json.Unmarshal(rec.Body.Bytes(), &resp)).To(Succeed())
		Expect(resp.Action).To(Equal(agentapi.ActionDeploy), "body: %s", rec.Body.String())
		Expect(resp.Plan).NotTo(BeNil())
		Expect(resp.Plan.OS).NotTo(BeNil())
		Expect(resp.Plan.OS.Partitions).To(HaveLen(1))
		Expect(resp.Plan.OS.Partitions[0].TorrentURL).To(ContainSubstring("10.9.9.20"),
			"the plan's torrent URL must point at the same Deployment/pod the reconciler created for this Machine's Site")
	})

	It("still counts demand but creates no Deployment for a Site with no seeder Subnet", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		site := fmt.Sprintf("site-noseed-%s", rand.String(5))
		subnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: site + "-sub", Namespace: image.Namespace},
			Spec: keziov1alpha1.SubnetSpec{
				SiteRef:         keziov1alpha1.NameRef{Name: site},
				CIDR:            "192.0.2.0/24",
				BootdServerIP:   "192.0.2.2",
				BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
				DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
			},
		}
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		siteObj := &keziov1alpha1.Site{
			ObjectMeta: metav1.ObjectMeta{Name: site, Namespace: image.Namespace},
			// No SeederSubnetRef: this Site runs no local seeder, so no
			// Deployment is created for it.
		}
		Expect(k8sClient.Create(ctx, siteObj)).To(Succeed())

		machine := createSeederTestMachineOnSubnet(ctx, image.Namespace, image.Name, subnet.Name, keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		siteIdentity := image.Namespace + "/" + site
		Expect(seederDeploymentsForImage(ctx, image)).NotTo(HaveKey(siteIdentity),
			"a Site with no seeder Subnet must get no Deployment")

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: siteIdentity, MachineCount: 1},
		), "demand must still be counted even though creation is gated on the seeder Subnet")

		cond := apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)
		Expect(cond).NotTo(BeNil(), "a Site with active demand but no seeder Subnet must be surfaced as degraded")
		Expect(cond.Reason).To(Equal("SeederSubnetRefUnset"))

		By("giving the Site a seederSubnetRef - the condition must clear and a Deployment must appear")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: site, Namespace: image.Namespace}, siteObj)).To(Succeed())
		siteObj.Spec.SeederSubnetRef = &keziov1alpha1.NameRef{Name: subnet.Name}
		Expect(k8sClient.Update(ctx, siteObj)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(seederDeploymentsForImage(ctx, image)).To(HaveKey(siteIdentity),
			"a Deployment must now be created once the Site resolves a seeder Subnet")
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)).To(BeNil(),
			"giving the Site a seeder Subnet must clear ImageConditionSeederDegraded, not leave it stuck")
	})

	It("creates no Deployment, but still succeeds and reports the missing Subnet by name, when a Site's seederSubnetRef names a Subnet that does not exist", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		site := fmt.Sprintf("site-dangling-%s", rand.String(5))
		subnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: site + "-sub", Namespace: image.Namespace},
			Spec: keziov1alpha1.SubnetSpec{
				SiteRef:         keziov1alpha1.NameRef{Name: site},
				CIDR:            "192.0.2.0/24",
				BootdServerIP:   "192.0.2.2",
				BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
				DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
			},
		}
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		siteObj := &keziov1alpha1.Site{
			ObjectMeta: metav1.ObjectMeta{Name: site, Namespace: image.Namespace},
			// Names a Subnet that is never created.
			Spec: keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: "ghost-seeder-subnet"}},
		}
		Expect(k8sClient.Create(ctx, siteObj)).To(Succeed())

		machine := createSeederTestMachineOnSubnet(ctx, image.Namespace, image.Name, subnet.Name, keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred(), "a dangling seederSubnetRef on one Site must not fail the whole reconcile")

		siteIdentity := image.Namespace + "/" + site
		Expect(seederDeploymentsForImage(ctx, image)).NotTo(HaveKey(siteIdentity),
			"a Site whose seederSubnetRef does not resolve must get no Deployment")

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		cond := apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)
		Expect(cond).NotTo(BeNil(), "a Site with active demand whose seederSubnetRef dangles must be surfaced as degraded, not left to a log line no status carries")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("SeederSubnetRefNotFound"),
			"a ref naming a Subnet that does not exist must not be reported as an unset ref - the remedy is to create that Subnet, not to set the field")
		Expect(cond.Message).To(ContainSubstring(siteIdentity))
		Expect(cond.Message).To(ContainSubstring(image.Namespace+"/ghost-seeder-subnet"),
			"the condition must name the Subnet that is missing, so the operator learns which name is wrong")

		By("creating the Subnet the ref names - the condition must clear and a Deployment must appear")
		ghost := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "ghost-seeder-subnet", Namespace: image.Namespace},
			Spec: keziov1alpha1.SubnetSpec{
				SiteRef:         keziov1alpha1.NameRef{Name: site},
				CIDR:            "192.0.2.0/24",
				BootdServerIP:   "192.0.2.2",
				BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
				DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
			},
		}
		Expect(k8sClient.Create(ctx, ghost)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ghost))).To(Succeed()) })

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(seederDeploymentsForImage(ctx, image)).To(HaveKey(siteIdentity),
			"a Deployment must now be created once the named Subnet exists")
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)).To(BeNil(),
			"creating the named Subnet must clear ImageConditionSeederDegraded, not leave it stuck")
	})

	It("is garbage collected when the Image itself is deleted", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		dep := seederDeploymentsForImage(ctx, image)["default/site-a"]
		Expect(dep.OwnerReferences).To(HaveLen(1))
		Expect(dep.OwnerReferences[0].Controller).NotTo(BeNil())
		Expect(*dep.OwnerReferences[0].Controller).To(BeTrue())
	})
})

// mustFixtureHash returns a fresh, valid partition content hash; this
// suite only needs an Image with a Ready state and non-empty
// status.partitions, never inspecting the content itself.
func mustFixtureHash() store.InfoHash {
	_, h := writeFixtureContent()
	return h
}

// createSeederDeploymentPod creates a ReplicaSet owned by dep and a Pod
// owned by that ReplicaSet, with status.podIP set - standing in for what
// a real Deployment controller and kubelet would otherwise produce
// (envtest runs neither).
func createSeederDeploymentPod(ctx context.Context, dep *appsv1.Deployment, podIP string) {
	trueVal := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", dep.Name, rand.String(5)),
			Namespace: dep.Namespace,
			Labels:    dep.Spec.Selector.MatchLabels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       dep.Name,
				UID:        dep.UID,
				Controller: &trueVal,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: dep.Spec.Selector,
			Template: dep.Spec.Template,
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, rs)).To(Succeed())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", rs.Name, rand.String(5)),
			Namespace: dep.Namespace,
			Labels:    dep.Spec.Selector.MatchLabels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       rs.Name,
				UID:        rs.UID,
				Controller: &trueVal,
			}},
		},
		Spec: dep.Spec.Template.Spec,
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.PodIP = podIP
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

var _ = Describe("Seeder Deployment content", func() {
	ctx := context.Background()

	// seederDeploymentContentTarget is the dial target
	// createSeederDeploymentPod's fixed pod IP resolves to, on the fixed
	// gRPC port every per-Image seeder container listens on.
	const seederDeploymentContentPodIP = "10.9.9.9"

	It("gives the Deployment a register container pointed at every partition mount, and never dials the pod itself", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		registry := newFakeSeederRegistry()

		r := &ImageReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			SeederDeployment: SeederDeploymentConfig{
				Image:      "ezio-seeder:test",
				TrackerURL: "http://tracker.kezio-system.svc:6969/announce",
				Dial:       registry.dial,
			},
		}
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		dep := seederDeploymentsForImage(ctx, image)["default/site-a"]

		createSeederDeploymentPod(ctx, &dep, seederDeploymentContentPodIP)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// The bytes a .torrent is built from live in the partition PVC,
		// which only the pod mounts, so registration belongs to the pod.
		Expect(registry.daemons).To(BeEmpty(), "the reconciler must not dial the seeder pod to add content")

		var register *corev1.Container
		for i := range dep.Spec.Template.Spec.Containers {
			if dep.Spec.Template.Spec.Containers[i].Name == "seeder-register" {
				register = &dep.Spec.Template.Spec.Containers[i]
			}
		}
		Expect(register).NotTo(BeNil(), "expected a seeder-register container alongside ezio")
		Expect(register.Command).To(Equal([]string{"/usr/local/bin/kezio-seeder-register"}))
		Expect(register.Env).To(ContainElement(corev1.EnvVar{Name: "CONTENT_ROOT", Value: ingest.ContentRoot}))

		// It scans the mounts rather than being told what to add, so it
		// must see exactly the same volumes ezio does.
		ezio := dep.Spec.Template.Spec.Containers[0]
		Expect(ezio.Name).To(Equal("ezio-seeder"))
		Expect(register.VolumeMounts).To(Equal(ezio.VolumeMounts))
	})
})
