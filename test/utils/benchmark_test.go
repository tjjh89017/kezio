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

package utils

import (
	"testing"
	"time"
)

func TestCapLeecherCount(t *testing.T) {
	tests := []struct {
		name             string
		requested        int
		contentSizeBytes int64
		availableBytes   int64
		safetyFraction   float64
		want             int
		wantErr          bool
	}{
		{
			name:             "plenty of disk keeps the requested count",
			requested:        10,
			contentSizeBytes: 1_000_000_000,   // 1 GB
			availableBytes:   100_000_000_000, // 100 GB
			safetyFraction:   0.2,
			want:             10,
		},
		{
			name:      "tight disk caps below the requested count",
			requested: 10,
			// 10 * 2GB = 20GB content, but only 8GB is usable after
			// the 20% safety margin on a 10GB disk (10GB*0.8=8GB), so
			// only 4 leechers' worth of 2GB content fits.
			contentSizeBytes: 2_000_000_000,
			availableBytes:   10_000_000_000,
			safetyFraction:   0.2,
			want:             4,
		},
		{
			name:             "zero available disk caps to zero",
			requested:        10,
			contentSizeBytes: 1_000_000_000,
			availableBytes:   0,
			safetyFraction:   0.2,
			want:             0,
		},
		{
			name:             "zero requested stays zero regardless of disk",
			requested:        0,
			contentSizeBytes: 1_000_000_000,
			availableBytes:   100_000_000_000,
			safetyFraction:   0.2,
			want:             0,
		},
		{
			name:             "no safety margin uses the full disk",
			requested:        5,
			contentSizeBytes: 1_000_000_000,
			availableBytes:   5_000_000_000,
			safetyFraction:   0,
			want:             5,
		},
		{
			name:             "negative requested is an error",
			requested:        -1,
			contentSizeBytes: 1_000_000_000,
			availableBytes:   1_000_000_000,
			safetyFraction:   0.2,
			wantErr:          true,
		},
		{
			name:             "non-positive content size is an error",
			requested:        1,
			contentSizeBytes: 0,
			availableBytes:   1_000_000_000,
			safetyFraction:   0.2,
			wantErr:          true,
		},
		{
			name:             "safety fraction of 1 is an error",
			requested:        1,
			contentSizeBytes: 1_000_000_000,
			availableBytes:   1_000_000_000,
			safetyFraction:   1,
			wantErr:          true,
		},
		{
			name:             "negative safety fraction is an error",
			requested:        1,
			contentSizeBytes: 1_000_000_000,
			availableBytes:   1_000_000_000,
			safetyFraction:   -0.1,
			wantErr:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CapLeecherCount(tt.requested, tt.contentSizeBytes, tt.availableBytes, tt.safetyFraction)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CapLeecherCount() expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("CapLeecherCount() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("CapLeecherCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSummarizeDurations(t *testing.T) {
	t.Run("empty input is an error", func(t *testing.T) {
		if _, err := SummarizeDurations(nil); err == nil {
			t.Fatal("expected an error for an empty input")
		}
	})

	t.Run("single value is its own min, median, and max", func(t *testing.T) {
		got, err := SummarizeDurations([]time.Duration{7 * time.Second})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := DurationSummary{Min: 7 * time.Second, Median: 7 * time.Second, Max: 7 * time.Second}
		if got != want {
			t.Errorf("SummarizeDurations() = %+v, want %+v", got, want)
		}
	})

	t.Run("odd count median is the middle sample, order independent", func(t *testing.T) {
		got, err := SummarizeDurations([]time.Duration{
			5 * time.Second, 1 * time.Second, 3 * time.Second,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := DurationSummary{Min: 1 * time.Second, Median: 3 * time.Second, Max: 5 * time.Second}
		if got != want {
			t.Errorf("SummarizeDurations() = %+v, want %+v", got, want)
		}
	})

	t.Run("even count median averages the two middle samples", func(t *testing.T) {
		got, err := SummarizeDurations([]time.Duration{
			1 * time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := DurationSummary{
			Min:    1 * time.Second,
			Median: (2*time.Second + 3*time.Second) / 2,
			Max:    4 * time.Second,
		}
		if got != want {
			t.Errorf("SummarizeDurations() = %+v, want %+v", got, want)
		}
	})

	t.Run("does not mutate the input slice order", func(t *testing.T) {
		durations := []time.Duration{5 * time.Second, 1 * time.Second, 3 * time.Second}
		original := append([]time.Duration(nil), durations...)
		if _, err := SummarizeDurations(durations); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i := range durations {
			if durations[i] != original[i] {
				t.Fatalf("SummarizeDurations() mutated its input: got %v, want %v", durations, original)
			}
		}
	})
}

func TestThroughputMBps(t *testing.T) {
	tests := []struct {
		name       string
		totalBytes int64
		wall       time.Duration
		want       float64
		wantErr    bool
	}{
		{
			name:       "one megabyte per second",
			totalBytes: 1_000_000,
			wall:       1 * time.Second,
			want:       1,
		},
		{
			name:       "ten gigabytes over ten seconds is one thousand MB/s",
			totalBytes: 10_000_000_000,
			wall:       10 * time.Second,
			want:       1000,
		},
		{
			name:       "zero bytes over positive wall time is zero throughput, not an error",
			totalBytes: 0,
			wall:       1 * time.Second,
			want:       0,
		},
		{
			name:       "negative bytes is an error",
			totalBytes: -1,
			wall:       1 * time.Second,
			wantErr:    true,
		},
		{
			name:       "zero wall time is an error",
			totalBytes: 1_000_000,
			wall:       0,
			wantErr:    true,
		},
		{
			name:       "negative wall time is an error",
			totalBytes: 1_000_000,
			wall:       -1 * time.Second,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ThroughputMBps(tt.totalBytes, tt.wall)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ThroughputMBps() expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("ThroughputMBps() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ThroughputMBps() = %v, want %v", got, tt.want)
			}
		})
	}
}
