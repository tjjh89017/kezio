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

package deploy

import (
	"testing"

	"github.com/tjjh89017/kezio/internal/seeder"
)

func TestShouldPauseTorrent(t *testing.T) {
	cases := []struct {
		name string
		t    seeder.Torrent
		want bool
	}{
		{
			name: "not finished",
			t:    seeder.Torrent{IsFinished: false, FinishedTime: 999, LastUpload: -1},
			want: false,
		},
		{
			name: "already paused",
			t:    seeder.Torrent{IsFinished: true, IsPaused: true, TotalPayloadUpload: 1000, TotalDone: 1},
			want: false,
		},
		{
			name: "finished but neither condition met yet",
			t:    seeder.Torrent{IsFinished: true, TotalDone: 100, TotalPayloadUpload: 100, FinishedTime: 5, LastUpload: 2},
			want: false,
		},
		{
			name: "upload exceeds 3x size",
			t:    seeder.Torrent{IsFinished: true, TotalDone: 100, TotalPayloadUpload: 301, FinishedTime: 1, LastUpload: 1},
			want: true,
		},
		{
			name: "upload exactly 3x size does not trigger (boundary is exclusive)",
			t:    seeder.Torrent{IsFinished: true, TotalDone: 100, TotalPayloadUpload: 300, FinishedTime: 1, LastUpload: 1},
			want: false,
		},
		{
			name: "finished long enough and idle long enough",
			t:    seeder.Torrent{IsFinished: true, TotalDone: 100, TotalPayloadUpload: 0, FinishedTime: 16, LastUpload: 16},
			want: true,
		},
		{
			name: "finished long enough but still uploading recently",
			t:    seeder.Torrent{IsFinished: true, TotalDone: 100, TotalPayloadUpload: 0, FinishedTime: 16, LastUpload: 5},
			want: false,
		},
		{
			name: "finished long enough and never uploaded (sentinel -1)",
			t:    seeder.Torrent{IsFinished: true, TotalDone: 100, TotalPayloadUpload: 0, FinishedTime: 16, LastUpload: -1},
			want: true,
		},
		{
			name: "finished but not long enough yet, even if idle",
			t:    seeder.Torrent{IsFinished: true, TotalDone: 100, TotalPayloadUpload: 0, FinishedTime: 10, LastUpload: 100},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldPauseTorrent(tc.t); got != tc.want {
				t.Errorf("shouldPauseTorrent(%+v) = %v, want %v", tc.t, got, tc.want)
			}
		})
	}
}

func TestAllStopped(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]seeder.Torrent
		want bool
	}{
		{name: "empty is not stopped", in: map[string]seeder.Torrent{}, want: false},
		{
			name: "one finished+paused",
			in:   map[string]seeder.Torrent{"a": {IsFinished: true, IsPaused: true}},
			want: true,
		},
		{
			name: "one still downloading",
			in: map[string]seeder.Torrent{
				"a": {IsFinished: true, IsPaused: true},
				"b": {IsFinished: false, IsPaused: false},
			},
			want: false,
		},
		{
			name: "finished but not yet paused",
			in:   map[string]seeder.Torrent{"a": {IsFinished: true, IsPaused: false}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allStopped(tc.in); got != tc.want {
				t.Errorf("allStopped(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestTorrentPercent(t *testing.T) {
	cases := []struct {
		name string
		t    seeder.Torrent
		want int32
	}{
		{name: "half done", t: seeder.Torrent{TotalDone: 50, Total: 100}, want: 50},
		{name: "zero total, not finished", t: seeder.Torrent{TotalDone: 0, Total: 0}, want: 0},
		{name: "zero total, finished", t: seeder.Torrent{IsFinished: true, Total: 0}, want: 100},
		{name: "fully done", t: seeder.Torrent{TotalDone: 100, Total: 100}, want: 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := torrentPercent(tc.t); got != tc.want {
				t.Errorf("torrentPercent(%+v) = %d, want %d", tc.t, got, tc.want)
			}
		})
	}
}
