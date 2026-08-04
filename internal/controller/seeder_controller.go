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
	"net"
	"sort"
	"strconv"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/store"
)

// seederRetryInterval is how soon a Reconcile that hit an error on at
// least one seeder endpoint (a dial failure, an RPC error) tries again.
// seederNoEndpointsRequeue is the (longer) fallback poll while no seeder
// endpoint is Ready yet; an EndpointSlice change also triggers a prompt
// reconcile, so this interval only covers the gap until the next one.
const (
	seederRetryInterval      = 15 * time.Second
	seederNoEndpointsRequeue = 30 * time.Second
)

// defaultGRPCPortName is the EndpointPort name the seeder Service's gRPC
// port is expected to carry (see config/seeder/service.yaml) when
// SeederConfig.GRPCPortName is left unset.
const defaultGRPCPortName = "grpc"

// SeederEZIOClient is the subset of internal/seeder.Client the reconciler
// needs. Production wiring uses *seeder.Client (via SeederConfig.Dial,
// which defaults to seeder.Dial); tests substitute a fake to exercise the
// reconcile loop against an in-memory ezio double without a network
// connection.
type SeederEZIOClient interface {
	AddTorrent(ctx context.Context, torrent []byte, savePath string, seedMode bool, maxUploads, maxConnections int32) error
	GetTorrentStatus(ctx context.Context, hashes []string) (map[string]seeder.Torrent, error)
	PauseTorrent(ctx context.Context, hash string) error
	ResumeTorrent(ctx context.Context, hash string) error
	Close() error
}

// SeederConfig configures the seeder reconciler. Its zero value keeps the
// reconciler inert: SetupWithManager does not register a controller at
// all, so an operator that never sets any SEEDER_* environment variable
// (every existing deployment, and the e2e suite) is completely
// unaffected by this package existing, the same gating shape
// IngestConfig uses for the ingest pipeline.
type SeederConfig struct {
	// TrackerURL is inserted as the "announce" field of every .torrent
	// this reconciler builds (see internal/store.BuildTorrentFile).
	TrackerURL string
	// StoreRoot is the store's local filesystem root, as mounted
	// read-only into the manager container out-of-band (see
	// config/seeder's README: this reconciler reads
	// contents/<hash>/torrent.info to build the .torrent bytes it hands
	// to ezio over AddTorrent, so it needs the same store volume the
	// ingest Job writes and the seeder Deployment serves from).
	StoreRoot string
	// ServiceNamespace and ServiceName locate the seeder Service. The
	// reconciler resolves it to individual Ready backends via its
	// EndpointSlices rather than dialing the Service's ClusterIP, so
	// every seeder pod (including ones mid-restart, which come back
	// with no torrents added) gets synced independently.
	ServiceNamespace string
	ServiceName      string
	// GRPCPortName is the EndpointPort name carrying the seeder's gRPC
	// port. Defaults to "grpc" when empty.
	GRPCPortName string
	// EzioTuning carries the cluster-wide default AddTorrent tuning
	// (MaxUploads/MaxConnections) applied to every content torrent this
	// reconciler adds. Nil (the default) falls back to
	// seeder.DefaultMaxUploads/seeder.DefaultMaxConnections - see
	// config/seeder/README.md's "WAN swarm tuning" section for how an
	// operator overrides it (SEEDER_MAX_UPLOADS/SEEDER_MAX_CONNECTIONS).
	// Only these two fields are consumed here: the seeder Deployment's
	// own daemon-level tuning (cache size, aio threads, port) is plain
	// container config (see config/seeder/ezio-seeder-deployment.yaml), not
	// per-torrent AddTorrent state this reconciler manages.
	EzioTuning *keziov1alpha1.MachineEzioTuning
	// Dial opens a client to one seeder endpoint's gRPC target
	// (host:port). Defaults to wrapping seeder.Dial when nil; tests
	// override it to hand back a fake.
	Dial func(target string) (SeederEZIOClient, error)
}

func (c SeederConfig) enabled() bool {
	return c.TrackerURL != "" && c.StoreRoot != "" && c.ServiceName != "" && c.ServiceNamespace != ""
}

func (c SeederConfig) dial(target string) (SeederEZIOClient, error) {
	if c.Dial != nil {
		return c.Dial(target)
	}
	return seeder.Dial(target)
}

func (c SeederConfig) portName() string {
	if c.GRPCPortName != "" {
		return c.GRPCPortName
	}
	return defaultGRPCPortName
}

// SeederReconciler ensures every Ready Image's partition-content torrents
// are added, with seed_mode set, on every Ready seeder endpoint, and that
// every other torrent a seeder endpoint already knows about is paused
// (never removed - ezio has no RemoveTorrent RPC). It is level-triggered
// and singleton-keyed: every Reconcile call (regardless of which Image
// or EndpointSlice triggered it) performs a full sync of "every Ready
// Image's content hashes" (the demand set) against "every seeder
// endpoint's current torrent set" via GetTorrentStatus - adding or
// resuming whatever the demand set wants and pausing whatever it does
// not - via syncTarget. This makes a seeder pod restart (which comes
// back with an empty torrent set) self-heal on the next reconcile
// without any special-casing: the diff against GetTorrentStatus
// naturally re-adds everything. See config/seeder/README.md's
// "Demand-driven torrent lifecycle" section for the scope and limits of
// this demand signal.
type SeederReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Seeder SeederConfig
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

// Reconcile performs one full sync pass. See SeederReconciler's doc
// comment for the level-triggered, singleton-keyed shape; req is not
// inspected.
func (r *SeederReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	if !r.Seeder.enabled() {
		return reconcile.Result{}, nil
	}
	logger := log.FromContext(ctx)

	hashes, err := r.readyContentHashes(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("list ready images: %w", err)
	}

	targets, err := r.seederTargets(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("list seeder endpoints: %w", err)
	}
	if len(targets) == 0 {
		logger.V(1).Info("no ready seeder endpoints, will retry", "service", r.Seeder.ServiceName)
		return reconcile.Result{RequeueAfter: seederNoEndpointsRequeue}, nil
	}

	var errs []error
	for _, target := range targets {
		if err := r.syncTarget(ctx, target, hashes); err != nil {
			logger.Error(err, "sync seeder endpoint", "target", target)
			errs = append(errs, fmt.Errorf("%s: %w", target, err))
		}
	}
	if len(errs) > 0 {
		return reconcile.Result{RequeueAfter: seederRetryInterval}, errors.Join(errs...)
	}
	return reconcile.Result{}, nil
}

// readyContentHashes returns the set of distinct partition-content info
// hashes (lowercase hex) across every Ready Image, deduplicated so
// content shared by more than one Image is only added once per seeder.
func (r *SeederReconciler) readyContentHashes(ctx context.Context) (map[string]struct{}, error) {
	var images keziov1alpha1.ImageList
	if err := r.List(ctx, &images); err != nil {
		return nil, err
	}

	hashes := make(map[string]struct{})
	for _, image := range images.Items {
		if image.Status.State != keziov1alpha1.ImageStateReady {
			continue
		}
		for _, p := range image.Status.Partitions {
			if p.InfoHash != "" {
				hashes[p.InfoHash] = struct{}{}
			}
		}
	}
	return hashes, nil
}

// seederTargets resolves the seeder Service to "ip:port" dial targets,
// one per Ready backend address on the configured gRPC port name.
func (r *SeederReconciler) seederTargets(ctx context.Context) ([]string, error) {
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices,
		client.InNamespace(r.Seeder.ServiceNamespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: r.Seeder.ServiceName},
	); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var targets []string
	for _, slice := range slices.Items {
		var port int32
		for _, p := range slice.Ports {
			if p.Name != nil && *p.Name == r.Seeder.portName() && p.Port != nil {
				port = *p.Port
				break
			}
		}
		if port == 0 {
			continue
		}
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready == nil || !*ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				target := net.JoinHostPort(addr, strconv.Itoa(int(port)))
				if _, ok := seen[target]; ok {
					continue
				}
				seen[target] = struct{}{}
				targets = append(targets, target)
			}
		}
	}
	sort.Strings(targets)
	return targets, nil
}

// syncTarget reconciles target's current torrent set (per
// GetTorrentStatus) against hashes, the demand set of every distinct
// content hash a Ready Image currently needs seeded:
//
//   - a hash in hashes missing from target gets added via AddTorrent
//     with seed_mode set;
//   - a hash in hashes present but paused gets resumed - closing the
//     loop on the pause below for an Image that goes Ready again after
//     having gone away;
//   - a hash present on target but no longer in hashes (its owning
//     Image is no longer Ready - deleted, or moved out of the Ready
//     state) gets paused, not removed: ezio has no RemoveTorrent RPC,
//     so a torrent's already-verified pieces simply stop being served
//     until (if ever) their content becomes Ready again.
//
// This is a coarse, Image-readiness-driven demand signal, not per-site
// or per-Machine demand tracking (whether some machine somewhere is
// actually still leeching a given hash right now); see
// config/seeder/README.md's "Demand-driven torrent lifecycle" section
// for what is and is not implemented here.
func (r *SeederReconciler) syncTarget(ctx context.Context, target string, hashes map[string]struct{}) error {
	c, err := r.Seeder.dial(target)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = c.Close() }()

	existing, err := c.GetTorrentStatus(ctx, nil)
	if err != nil {
		return fmt.Errorf("get torrent status: %w", err)
	}

	for hash := range hashes {
		t, ok := existing[hash]
		if !ok {
			if err := r.addContent(ctx, c, hash); err != nil {
				return fmt.Errorf("add content %s: %w", hash, err)
			}
			continue
		}
		if t.IsPaused {
			if err := c.ResumeTorrent(ctx, hash); err != nil {
				return fmt.Errorf("resume content %s: %w", hash, err)
			}
		}
	}

	for hash, t := range existing {
		if _, wanted := hashes[hash]; wanted {
			continue
		}
		if t.IsPaused {
			continue
		}
		if err := c.PauseTorrent(ctx, hash); err != nil {
			return fmt.Errorf("pause content %s: %w", hash, err)
		}
	}
	return nil
}

// addContent reads hash's torrent.info from the store, builds its
// .torrent bytes with the configured tracker URL, and adds it to c with
// seed_mode set. save_path is the content's directory itself
// (store.ContentDir): ezio's -F (file) mode leaves libtorrent's default
// multi-file storage in place instead of its own raw-disk I/O path, and
// that storage resolves every torrent file entry as
// "<save_path>/<info dict name>/<file path>" per BEP3 - not
// "<save_path>/<file path>" directly. BuildInfoDict's info dict name is
// the fixed contentName ("content"), and its file entries are the
// extents' own hex-offset leaf names (internal/store.ExtentFileName), so
// the bytes libtorrent actually opens live at
// "<save_path>/content/<hex offset>" - store.ContentDataDir(contentDir),
// which is exactly where the store publishes them (see
// internal/store.NestExtentFiles). save_path itself stays contentDir:
// only the physical extent files moved one level deeper to match what
// the torrent's own metadata already implies.
func (r *SeederReconciler) addContent(ctx context.Context, c SeederEZIOClient, hash string) error {
	infoHash, err := store.ParseInfoHash(hash)
	if err != nil {
		return err
	}
	contentDir := store.ContentDir(r.Seeder.StoreRoot, infoHash)

	info, err := store.LoadContentDirTorrentInfo(contentDir)
	if err != nil {
		return fmt.Errorf("load torrent.info: %w", err)
	}
	torrentBytes, err := store.BuildTorrentFile(info, r.Seeder.TrackerURL)
	if err != nil {
		return fmt.Errorf("build torrent file: %w", err)
	}
	return c.AddTorrent(ctx, torrentBytes, contentDir, true,
		seeder.ResolveMaxUploads(r.Seeder.EzioTuning), seeder.ResolveMaxConnections(r.Seeder.EzioTuning))
}

// alwaysSyncRequest is the fixed reconcile.Request every watched event
// maps to; Reconcile ignores its content (see SeederReconciler's doc
// comment), so all that matters is that some request is enqueued.
var alwaysSyncRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "seeder-sync"}}

func mapToSeederSync(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{alwaysSyncRequest}
}

// SetupWithManager registers the seeder controller, unless r.Seeder is
// not enabled (see SeederConfig.enabled), in which case it returns nil
// without registering anything - the inert default.
func (r *SeederReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if !r.Seeder.enabled() {
		return nil
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("seeder").
		Watches(&keziov1alpha1.Image{}, handler.EnqueueRequestsFromMapFunc(mapToSeederSync)).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(mapToSeederSync)).
		Complete(r)
}
