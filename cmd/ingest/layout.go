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

package main

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/ingest"
)

// inClusterLayoutWriter implements ingest.LayoutWriter, building its
// controller-runtime client lazily on first use rather than in
// buildFromEnv: config.GetConfig only succeeds inside a real pod (or
// with KUBECONFIG set), so constructing it eagerly would break every
// unit test that calls buildFromEnv outside a cluster even when the run
// under test never reaches the point of writing the ImageLayout (see
// cmd/ingest/main_test.go).
type inClusterLayoutWriter struct {
	namespace string
	owner     ingest.ImageOwnerRef
}

// newInClusterLayoutWriter builds the LayoutWriter buildFromEnv wires
// for a real ingest run.
func newInClusterLayoutWriter(namespace string, owner ingest.ImageOwnerRef) ingest.LayoutWriter {
	return &inClusterLayoutWriter{namespace: namespace, owner: owner}
}

func (w *inClusterLayoutWriter) WriteLayout(ctx context.Context, sfdiskJSON string) error {
	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster kubernetes config: %w", err)
	}
	scheme, err := keziov1alpha1.SchemeBuilder.Build()
	if err != nil {
		return fmt.Errorf("build kezio scheme: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}
	return ingest.WriteImageLayout(ctx, c, w.namespace, w.owner, sfdiskJSON)
}
