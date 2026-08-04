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
	"fmt"
	"os"
	"path/filepath"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/store"
)

// fakeDaemon stands in for one ezio daemon's torrent set. It is shared by
// every fakeSeederClient dialed against the same target, so it behaves
// like a real daemon across repeated Reconcile calls: AddTorrent adds
// durably, GetTorrentStatus reflects everything added so far. Tests
// simulate a seeder pod restart by replacing a target's daemon with a
// fresh (empty) one.
type fakeDaemon struct {
	mu       sync.Mutex
	torrents map[string]seeder.Torrent
	addCount int
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{torrents: make(map[string]seeder.Torrent)}
}

type fakeSeederClient struct{ d *fakeDaemon }

func (c fakeSeederClient) AddTorrent(_ context.Context, _ []byte, savePath string, _ bool) error {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.addCount++
	// The content hash is the save_path's leaf directory name (see
	// addContent: save_path is store.ContentDir(root, hash)).
	hash := filepath.Base(savePath)
	c.d.torrents[hash] = seeder.Torrent{Hash: hash}
	return nil
}

func (c fakeSeederClient) GetTorrentStatus(_ context.Context, hashes []string) (map[string]seeder.Torrent, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	if len(hashes) == 0 {
		out := make(map[string]seeder.Torrent, len(c.d.torrents))
		for k, v := range c.d.torrents {
			out[k] = v
		}
		return out, nil
	}
	out := make(map[string]seeder.Torrent)
	for _, h := range hashes {
		if t, ok := c.d.torrents[h]; ok {
			out[h] = t
		}
	}
	return out, nil
}

func (c fakeSeederClient) Close() error { return nil }

// fakeSeederRegistry hands out (and remembers) one *fakeDaemon per dial
// target, so a test can dial the "same daemon" repeatedly across
// Reconcile calls the way a real seeder Service would.
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

// restart replaces target's daemon with a fresh, empty one, simulating
// the seeder pod (and the in-memory ezio process it runs) restarting.
func (r *fakeSeederRegistry) restart(target string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.daemons[target] = newFakeDaemon()
}

// writeFixtureContent writes a minimal, valid content directory (a
// torrent.info plus its one extent file, both required by
// store.LoadContentDirTorrentInfo/store.ValidateContentDir) under a fresh
// temp store root, and returns the root and the content's info hash.
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
	Expect(os.MkdirAll(dir, 0o755)).To(Succeed())

	infoFile, err := os.Create(store.ContentTorrentInfoPath(root, hash)) //nolint:gosec // test fixture path
	Expect(err).NotTo(HaveOccurred())
	Expect(store.WriteTorrentInfo(infoFile, info)).To(Succeed())
	Expect(infoFile.Close()).To(Succeed())

	extentPath := store.ContentExtentPath(root, hash, 0)
	Expect(os.WriteFile(extentPath, make([]byte, store.PieceSize), 0o644)).To(Succeed()) //nolint:gosec // test fixture path

	return root, hash
}

// createReadyImage creates an Image that is immediately Ready with one
// partition carrying hash, bypassing the ingest state machine (this
// suite only needs a Ready Image with a known info hash, not a full
// ingest walk - that is image_controller_test.go's concern).
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

// createSeederEndpointSlice creates a Ready EndpointSlice for the seeder
// Service in svcNamespace/svcName, with one endpoint at address on the
// named gRPC port, and returns the dial target ("address:port") it
// resolves to.
func createSeederEndpointSlice(ctx context.Context, svcNamespace, svcName, namePrefix, address string, port int32) string {
	ready := true
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", namePrefix, rand.String(5)),
			Namespace: svcNamespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: svcName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{address},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name: ptr.To(defaultGRPCPortName),
			Port: ptr.To(port),
		}},
	}
	Expect(k8sClient.Create(ctx, slice)).To(Succeed())
	return fmt.Sprintf("%s:%d", address, port)
}

var _ = Describe("Seeder Controller", func() {
	ctx := context.Background()

	It("does nothing when SEEDER_* configuration is unset", func() {
		r := &SeederReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(r.Seeder.enabled()).To(BeFalse())

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "irrelevant"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))
	})

	Context("with a configured tracker, store, and seeder Service", func() {
		var (
			r         *SeederReconciler
			registry  *fakeSeederRegistry
			storeRoot string
			namespace string
		)

		BeforeEach(func() {
			namespace = "default"
			registry = newFakeSeederRegistry()
			storeRoot, _ = os.MkdirTemp("", "seeder-store-root-*")
			DeferCleanup(func() { _ = os.RemoveAll(storeRoot) })

			r = &SeederReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Seeder: SeederConfig{
					TrackerURL:       "http://tracker.kezio-system.svc:6969/announce",
					StoreRoot:        storeRoot,
					ServiceNamespace: namespace,
					ServiceName:      fmt.Sprintf("seeder-svc-%s", rand.String(5)),
					Dial:             registry.dial,
				},
			}
		})

		It("adds a Ready Image's content to a Ready seeder endpoint", func() {
			fixtureRoot, hash := writeFixtureContent()
			r.Seeder.StoreRoot = fixtureRoot

			image := createReadyImage(ctx, hash)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, image) })

			target := createSeederEndpointSlice(ctx, namespace, r.Seeder.ServiceName, "seeder-eps-1", "10.0.0.1", 50051)

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "seeder-sync"}})
			Expect(err).NotTo(HaveOccurred())

			daemon := registry.daemons[target]
			Expect(daemon).NotTo(BeNil())
			Expect(daemon.torrents).To(HaveKey(hash.String()))
		})

		It("does not re-add content already present on the endpoint (no duplicate adds)", func() {
			fixtureRoot, hash := writeFixtureContent()
			r.Seeder.StoreRoot = fixtureRoot

			image := createReadyImage(ctx, hash)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, image) })

			target := createSeederEndpointSlice(ctx, namespace, r.Seeder.ServiceName, "seeder-eps-1", "10.0.0.2", 50051)

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "seeder-sync"}}
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			daemon := registry.daemons[target]
			Expect(daemon.addCount).To(Equal(1))
		})

		It("re-adds everything after a seeder pod restart (empty GetTorrentStatus)", func() {
			fixtureRoot, hash := writeFixtureContent()
			r.Seeder.StoreRoot = fixtureRoot

			image := createReadyImage(ctx, hash)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, image) })

			target := createSeederEndpointSlice(ctx, namespace, r.Seeder.ServiceName, "seeder-eps-1", "10.0.0.3", 50051)

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "seeder-sync"}}
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(registry.daemons[target].addCount).To(Equal(1))

			registry.restart(target)
			Expect(registry.daemons[target].torrents).To(BeEmpty())

			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(registry.daemons[target].addCount).To(Equal(1))
			Expect(registry.daemons[target].torrents).To(HaveKey(hash.String()))
		})

		It("requeues without error when no seeder endpoint is Ready yet", func() {
			fixtureRoot, hash := writeFixtureContent()
			r.Seeder.StoreRoot = fixtureRoot

			image := createReadyImage(ctx, hash)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, image) })

			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "seeder-sync"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		})

		It("ignores a not-Ready endpoint", func() {
			fixtureRoot, hash := writeFixtureContent()
			r.Seeder.StoreRoot = fixtureRoot

			image := createReadyImage(ctx, hash)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, image) })

			notReady := false
			slice := &discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "seeder-eps-notready",
					Namespace: namespace,
					Labels:    map[string]string{discoveryv1.LabelServiceName: r.Seeder.ServiceName},
				},
				AddressType: discoveryv1.AddressTypeIPv4,
				Endpoints: []discoveryv1.Endpoint{{
					Addresses:  []string{"10.0.0.4"},
					Conditions: discoveryv1.EndpointConditions{Ready: &notReady},
				}},
				Ports: []discoveryv1.EndpointPort{{
					Name: ptr.To(defaultGRPCPortName),
					Port: ptr.To(int32(50051)),
				}},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())

			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "seeder-sync"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(registry.daemons).To(BeEmpty())
		})
	})
})
