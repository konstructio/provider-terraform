/*
Copyright 2026 The Crossplane Authors.

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

package shard

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestNewFleet(t *testing.T) {
	cases := map[string]struct {
		reason string
		active []string
		want   []string
	}{
		"SortsByIndexNotString": {
			reason: "Ties break toward the lowest index, so ordering must be numeric - shard-10 sorts after shard-9, not before it.",
			active: []string{"shard-10", "shard-2", "shard-9", "shard-0"},
			want:   []string{"shard-0", "shard-2", "shard-9", "shard-10"},
		},
		"DropsNonCanonicalNames": {
			reason: "A hand-written label must not become a placement target.",
			active: []string{"shard-0", "shard-1-old", "shard-", "worker-1", "shard-01"},
			want:   []string{"shard-0"},
		},
		"Deduplicates": {
			reason: "Two Deployments labelled for the same shard should count once.",
			active: []string{"shard-0", "shard-0", "shard-1"},
			want:   []string{"shard-0", "shard-1"},
		},
		"Empty": {
			reason: "Every shard scaled to zero leaves nowhere to place, which callers must handle.",
			active: []string{},
			want:   []string{},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, NewFleet(tc.active).ActiveShards()); diff != "" {
				t.Errorf("NewFleet(...).ActiveShards(): -want, +got:\n%s\n%s", diff, tc.reason)
			}
		})
	}
}

func TestFleetActive(t *testing.T) {
	f := NewFleet([]string{"shard-0", "shard-1", "shard-2"})

	cases := map[string]struct {
		reason string
		shard  string
		want   bool
	}{
		"Active":      {reason: "A shard with a scaled-up Deployment is placeable.", shard: "shard-0", want: true},
		"NotDeclared": {reason: "A shard with no Deployment - pruned, or scaled to zero - is not placeable.", shard: "shard-3", want: false},
		"Unlabelled":  {reason: "The empty label always needs placement.", shard: "", want: false},
		"Malformed":   {reason: "A hand-written label is treated as unplaced rather than trusted.", shard: "shard-1-old", want: false},
		"LeadingZero": {reason: "shard-01 is not canonical and must not alias shard-1.", shard: "shard-01", want: false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := f.Active(tc.shard); got != tc.want {
				t.Errorf("Active(%q): want %v, got %v\n%s", tc.shard, tc.want, got, tc.reason)
			}
		})
	}
}

func TestShardIndex(t *testing.T) {
	cases := map[string]struct {
		in     string
		wantI  int
		wantOK bool
	}{
		"Zero":        {in: "shard-0", wantI: 0, wantOK: true},
		"TwoDigits":   {in: "shard-12", wantI: 12, wantOK: true},
		"LeadingZero": {in: "shard-01", wantOK: false},
		"NoIndex":     {in: "shard-", wantOK: false},
		"NoPrefix":    {in: "0", wantOK: false},
		"Negative":    {in: "shard--1", wantOK: false},
		"Suffix":      {in: "shard-1-old", wantOK: false},
		"Empty":       {in: "", wantOK: false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			i, ok := shardIndex(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("shardIndex(%q): want ok=%v, got %v", tc.in, tc.wantOK, ok)
			}
			if ok && i != tc.wantI {
				t.Errorf("shardIndex(%q): want %d, got %d", tc.in, tc.wantI, i)
			}
		})
	}
}
