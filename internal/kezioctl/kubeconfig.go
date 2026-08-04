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

package kezioctl

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// Scheme is the runtime.Scheme kezioctl builds its Kubernetes client
// with. It only needs the kezio API group (plus the client-go default
// types clientcmd itself may touch): kezioctl talks to Images and
// Machines only, never core workload objects.
var Scheme = runtime.NewScheme()

func init() {
	if err := keziov1alpha1.AddToScheme(Scheme); err != nil {
		panic(fmt.Sprintf("register kezio API types: %v", err))
	}
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
	return restConfig, namespace, nil
}

// NewClient builds a controller-runtime client.Client for the kezio API
// group, resolving its REST config and default namespace the same way
// LoadRESTConfig does. Every kezioctl command builds its Kubernetes
// access through this client.Client interface (never a generated
// typed clientset) so the same command logic runs unmodified against
// either a real cluster or, in tests, a
// sigs.k8s.io/controller-runtime/pkg/client/fake client.
func NewClient(kubeconfigPath string) (client.Client, string, error) {
	restConfig, namespace, err := LoadRESTConfig(kubeconfigPath)
	if err != nil {
		return nil, "", err
	}
	c, err := client.New(restConfig, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, "", fmt.Errorf("build Kubernetes client: %w", err)
	}
	return c, namespace, nil
}
