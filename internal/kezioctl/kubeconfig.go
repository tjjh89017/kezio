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

package kezioctl

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// apiWarningCode is the only HTTP Warning code Kubernetes sends
// (RFC 7234's "miscellaneous persistent warning").
const apiWarningCode = 299

// apiWarningHandler prints an API server warning to stderr, the way
// kubectl does, keeping it out of a command's own stdout.
//
// Installing any handler at all is what keeps controller-runtime from
// falling back to its logger-backed one: that fallback needs a logger
// this CLI never sets, so it discards the warning and dumps a goroutine
// stack complaining about the missing logger in its place - losing the
// message the server actually sent.
type apiWarningHandler struct{ w io.Writer }

var _ rest.WarningHandlerWithContext = apiWarningHandler{}

func (h apiWarningHandler) HandleWarningHeaderWithContext(_ context.Context, code int, _ string, text string) {
	if code != apiWarningCode || text == "" {
		return
	}
	// A warning nobody can print is not worth failing a command over.
	_, _ = fmt.Fprintln(h.w, "Warning:", text)
}

// Scheme is the runtime.Scheme kezioctl builds its Kubernetes client with.
// It only needs the kezio API group (plus the client-go default types
// clientcmd itself may touch): kezioctl talks to Images (and, later,
// Machines) only, never core workload objects.
var Scheme = runtime.NewScheme()

// restMapper is a static meta.RESTMapper covering every Kind Scheme
// knows about, derived from Scheme itself instead of from cluster
// discovery.
//
// controller-runtime's client.New defaults to a discovery-backed
// RESTMapper that, on first use, asks the API server for every version
// the kezio.kojuro.date group has ever advertised - including one this
// CLI no longer uses - and fails the whole lookup if any of them errors
// out (see sigs.k8s.io/controller-runtime/pkg/client/apiutil.mapper.
// addKnownGroupAndReload's aggregated-discovery branch). A CRD that
// still lists a deprecated, unserved version turns that into an
// "image upload --wait" failure over an API version this command was
// never asking for. Since every Kind this client can be asked to
// resolve is already compiled into Scheme, there is nothing to
// discover: read the mapping off Scheme instead, so a Kind added to
// api/v1alpha3 is picked up without also having to update this file.
//
// Built in init(), after AddToScheme, rather than in this var's own
// initializer: package-level var initializers all run before any
// init() func, so building it from Scheme at var-init time would read
// Scheme before AddToScheme has registered anything into it.
var restMapper meta.RESTMapper

func init() {
	if err := keziov1alpha3.AddToScheme(Scheme); err != nil {
		panic(fmt.Sprintf("register kezio API types: %v", err))
	}
	restMapper = newRESTMapperFromScheme(Scheme)
}

// newRESTMapperFromScheme builds a RESTMapper entry for every "real"
// Kind scheme registers under keziov1alpha3.GroupVersion: every
// registered Kind other than a List (its base Kind already covers
// lookups - see client_rest_resources.go stripping the "List" suffix
// before mapping) and the handful of meta.k8s.io machinery types
// (WatchEvent, *Options) that metav1.AddToGroupVersion registers into
// every GroupVersion a scheme.Builder is used for and that are never
// looked up through a RESTMapper.
//
// Every kezio.kojuro.date CRD is namespace-scoped today (see `scope:
// Namespaced` in every config/crd/bases/*.yaml); nothing on the Go type
// or in Scheme records that, so it is asserted here rather than
// guessed. A cluster-scoped kezio Kind would need this function to
// consult something more than Scheme.
func newRESTMapperFromScheme(scheme *runtime.Scheme) meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{keziov1alpha3.GroupVersion})
	for gvk := range scheme.AllKnownTypes() {
		if gvk.GroupVersion() != keziov1alpha3.GroupVersion {
			continue
		}
		if gvk.Kind == "WatchEvent" || strings.HasSuffix(gvk.Kind, "List") || strings.HasSuffix(gvk.Kind, "Options") {
			continue
		}
		m.Add(gvk, meta.RESTScopeNamespace)
	}
	return m
}

// LoadRESTConfig resolves a *rest.Config and a default namespace the same
// way kubectl does: an explicit --kubeconfig path, if given, is used
// as-is; otherwise the standard loading rules apply (the KUBECONFIG
// environment variable, split on the OS path separator, falling back to
// ~/.kube/config). The returned namespace is the resolved context's
// namespace, or "default" when the kubeconfig does not set one.
func LoadRESTConfig(kubeconfigPath string) (*rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})

	restConfig, err := loader.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}

	namespace, _, err := loader.Namespace()
	if err != nil {
		namespace = "default"
	}
	restConfig.WarningHandlerWithContext = apiWarningHandler{w: os.Stderr}
	return restConfig, namespace, nil
}

// NewClient builds a controller-runtime client.Client for the kezio API
// group, resolving its REST config and default namespace the same way
// LoadRESTConfig does. Every kezioctl command builds its Kubernetes access
// through this client.Client interface (never a generated typed
// clientset) so the same command logic runs unmodified against either a
// real cluster or, in tests, a
// sigs.k8s.io/controller-runtime/pkg/client/fake client.
func NewClient(kubeconfigPath string) (client.Client, string, error) {
	restConfig, namespace, err := LoadRESTConfig(kubeconfigPath)
	if err != nil {
		return nil, "", err
	}
	c, err := client.New(restConfig, client.Options{Scheme: Scheme, Mapper: restMapper})
	if err != nil {
		return nil, "", fmt.Errorf("build Kubernetes client: %w", err)
	}
	return c, namespace, nil
}
