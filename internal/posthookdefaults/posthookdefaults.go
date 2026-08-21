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

// Package posthookdefaults defines the kezio-default-finalize PostHook: the
// spec the manager ships and keeps in place (see Ensurer), and the spec the
// deploy plan builder attaches to a Machine that sets no postHookRefs.
package posthookdefaults

import (
	"os"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// DefaultFinalizeHookName is the name of the shipped default PostHook.
const DefaultFinalizeHookName = "kezio-default-finalize"

// Spec returns the shipped kezio-default-finalize PostHook spec: mkswap
// then efibootmgr, both Linux-only builtins. Every call returns an
// independent value safe for a caller to mutate.
func Spec() keziov1alpha2.PostHookSpec {
	return keziov1alpha2.PostHookSpec{
		Steps: []keziov1alpha2.PostHookStep{
			{
				OSFamily: keziov1alpha2.OSFamilyLinux,
				Builtin:  &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepMkswap},
			},
			{
				OSFamily: keziov1alpha2.OSFamilyLinux,
				Builtin:  &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepEfibootmgr},
			},
		},
	}
}

// Namespace returns the manager's own namespace from the POD_NAMESPACE
// downward-API env var (see config/manager/manager.yaml), and false when it
// is unset - a local `make run` outside a Pod, where the default PostHook
// has no namespace to live in.
func Namespace() (string, bool) {
	ns := os.Getenv("POD_NAMESPACE")
	return ns, ns != ""
}
