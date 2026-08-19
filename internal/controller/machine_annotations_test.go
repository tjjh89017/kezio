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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func TestIsPaused(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"absent", nil, false},
		{"present empty value", map[string]string{keziov1alpha2.MachineAnnotationPaused: ""}, true},
		{"present non-empty value", map[string]string{keziov1alpha2.MachineAnnotationPaused: "anything"}, true},
		{"other annotation only", map[string]string{"other": "x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := &keziov1alpha2.Machine{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			if got := isPaused(machine); got != tc.want {
				t.Errorf("isPaused() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsDetached(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"absent", nil, false},
		{"present", map[string]string{keziov1alpha2.MachineAnnotationDetached: ""}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := &keziov1alpha2.Machine{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			if got := isDetached(machine); got != tc.want {
				t.Errorf("isDetached() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReInspectRequested(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"absent", nil, false},
		{"present empty value", map[string]string{keziov1alpha2.MachineAnnotationReInspect: ""}, true},
		{"present non-empty value", map[string]string{keziov1alpha2.MachineAnnotationReInspect: "anything"}, true},
		{"other annotation only", map[string]string{"other": "x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := &keziov1alpha2.Machine{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			if got := reInspectRequested(machine); got != tc.want {
				t.Errorf("reInspectRequested() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInspectDisabled(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"absent", nil, false},
		{"exactly true", map[string]string{keziov1alpha2.MachineAnnotationInspectDisable: annotationValueTrue}, true},
		{"non-true value", map[string]string{keziov1alpha2.MachineAnnotationInspectDisable: "yes"}, false},
		{"empty value", map[string]string{keziov1alpha2.MachineAnnotationInspectDisable: ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := &keziov1alpha2.Machine{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			if got := inspectDisabled(machine); got != tc.want {
				t.Errorf("inspectDisabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsRebootAnnotationKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{keziov1alpha2.MachineAnnotationRebootPrefix, true},
		{keziov1alpha2.MachineAnnotationRebootPrefix + "-fleetctl", true},
		{keziov1alpha2.MachineAnnotationRebootPrefix + "-", true},
		{keziov1alpha2.MachineAnnotationRebootPrefix + "x", false},
		{"kezio.kojuro.date/other", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got := isRebootAnnotationKey(tc.key); got != tc.want {
				t.Errorf("isRebootAnnotationKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestParseRebootAnnotation(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    keziov1alpha2.MachineRebootMode
		wantErr bool
	}{
		{"empty value defaults to soft", "", keziov1alpha2.MachineRebootModeSoft, false},
		{"explicit soft", `{"mode":"soft"}`, keziov1alpha2.MachineRebootModeSoft, false},
		{"explicit hard", `{"mode":"hard"}`, keziov1alpha2.MachineRebootModeHard, false},
		{"invalid json falls back to soft with an error", `{not json`, keziov1alpha2.MachineRebootModeSoft, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRebootAnnotation(tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseRebootAnnotation(%q) error = %v, wantErr %v", tc.value, err, tc.wantErr)
			}
			if got.Mode != tc.want {
				t.Errorf("parseRebootAnnotation(%q) mode = %q, want %q", tc.value, got.Mode, tc.want)
			}
		})
	}
}

func TestRebootRequested(t *testing.T) {
	cases := []struct {
		name          string
		annotations   map[string]string
		wantRequested bool
		wantHard      bool
	}{
		{"no annotations", nil, false, false},
		{
			"suffixless soft only",
			map[string]string{keziov1alpha2.MachineAnnotationRebootPrefix: `{"mode":"soft"}`},
			true, false,
		},
		{
			"suffixed hard only",
			map[string]string{keziov1alpha2.MachineAnnotationRebootPrefix + "-fleetctl": `{"mode":"hard"}`},
			true, true,
		},
		{
			"suffixless soft plus suffixed hard: hard wins",
			map[string]string{
				keziov1alpha2.MachineAnnotationRebootPrefix:        `{"mode":"soft"}`,
				keziov1alpha2.MachineAnnotationRebootPrefix + "-a": `{"mode":"hard"}`,
			},
			true, true,
		},
		{
			"invalid json on one holder still counts as requested and soft",
			map[string]string{keziov1alpha2.MachineAnnotationRebootPrefix: `{not json`},
			true, false,
		},
		{
			"invalid json on one holder does not mask a hard request from another",
			map[string]string{
				keziov1alpha2.MachineAnnotationRebootPrefix:        `{not json`,
				keziov1alpha2.MachineAnnotationRebootPrefix + "-b": `{"mode":"hard"}`,
			},
			true, true,
		},
		{
			"unrelated annotation ignored",
			map[string]string{"kezio.kojuro.date/other": `{"mode":"hard"}`},
			false, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := &keziov1alpha2.Machine{ObjectMeta: metav1.ObjectMeta{Name: "m1", Annotations: tc.annotations}}
			requested, hard := rebootRequested(context.Background(), machine)
			if requested != tc.wantRequested {
				t.Errorf("rebootRequested() requested = %v, want %v", requested, tc.wantRequested)
			}
			if hard != tc.wantHard {
				t.Errorf("rebootRequested() hard = %v, want %v", hard, tc.wantHard)
			}
		})
	}
}
