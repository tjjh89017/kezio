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
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/sitederive"
)

// lazySiteResolution resolves machine's seeder placement
// (sitederive.Resolve) at most once per Build call, and only when a slot
// with a contentRef actually asks for it - a Machine deploying an Image
// with no content slots at all (every slot blank/swap) never needs a
// Site to resolve, and must not fail Build over an unrelated dangling
// subnetRef/siteRef it never uses.
type lazySiteResolution struct {
	client  client.Reader
	machine *keziov1alpha2.Machine

	once sync.Once
	res  sitederive.Resolution
	err  error
}

// resolve returns the memoized sitederive.Resolve result, computing it on
// the first call.
func (l *lazySiteResolution) resolve(ctx context.Context) (sitederive.Resolution, error) {
	l.once.Do(func() {
		l.res, l.err = sitederive.Resolve(ctx, l.client, l.machine)
	})
	return l.res, l.err
}
