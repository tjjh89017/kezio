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

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// conditionObservedGenerationStale reports whether cond's
// ObservedGeneration is behind generation - the reader-side half of
// kezio's cross-reference contract (see aggregateSlotContents' doc
// comment for the contract in full: a referenced kind exposes Ready/Valid
// conditions carrying observedGeneration, and a reader that sees a stale
// one must retry rather than act on it). True means the referenced
// object's own controller has not yet reconciled its latest generation,
// so cond describes an obsolete observation. A nil cond is never stale by
// this definition - "condition absent" is a distinct case the caller
// checks for itself before calling this.
func conditionObservedGenerationStale(cond *metav1.Condition, generation int64) bool {
	return cond != nil && cond.ObservedGeneration != generation
}
