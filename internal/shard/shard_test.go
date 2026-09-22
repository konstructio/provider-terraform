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

func TestParseConfig(t *testing.T) {
	type args struct {
		data map[string]string
	}
	type want struct {
		cfg     Config
		wantErr bool
	}
	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"CountOnly": {
			reason: "A bare shardCount yields that many shards and nothing draining.",
			args:   args{data: map[string]string{KeyShardCount: "4"}},
			want:   want{cfg: Config{Count: 4, Draining: map[string]bool{}}},
		},
		"WithDraining": {
			reason: "Draining shards are parsed out of the comma separated list.",
			args:   args{data: map[string]string{KeyShardCount: "4", KeyDraining: "shard-3"}},
			want:   want{cfg: Config{Count: 4, Draining: map[string]bool{"shard-3": true}}},
		},
		"DrainingWhitespaceAndEmpties": {
			reason: "Whitespace and empty entries in draining are ignored rather than rejected.",
			args:   args{data: map[string]string{KeyShardCount: "4", KeyDraining: " shard-2 , ,shard-3,"}},
			want:   want{cfg: Config{Count: 4, Draining: map[string]bool{"shard-2": true, "shard-3": true}}},
		},
		"EmptyDrainingIsFine": {
			reason: "An empty draining value is the normal steady state.",
			args:   args{data: map[string]string{KeyShardCount: "2", KeyDraining: ""}},
			want:   want{cfg: Config{Count: 2, Draining: map[string]bool{}}},
		},
		"MissingCount": {
			reason: "shardCount is required; guessing a default would silently misplace everything.",
			args:   args{data: map[string]string{KeyDraining: "shard-1"}},
			want:   want{wantErr: true},
		},
		"UnparseableCount": {
			reason: "A non-numeric shardCount is an operator error, not a reason to proceed.",
			args:   args{data: map[string]string{KeyShardCount: "four"}},
			want:   want{wantErr: true},
		},
		"ZeroCount": {
			reason: "Zero shards would leave every Workspace unreconciled.",
			args:   args{data: map[string]string{KeyShardCount: "0"}},
			want:   want{wantErr: true},
		},
		"NegativeCount": {
			reason: "A negative shardCount is nonsense.",
			args:   args{data: map[string]string{KeyShardCount: "-1"}},
			want:   want{wantErr: true},
		},
		"MalformedDrainingName": {
			reason: "A typo in draining must fail loudly, or a drain silently never happens.",
			args:   args{data: map[string]string{KeyShardCount: "4", KeyDraining: "shard3"}},
			want:   want{wantErr: true},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseConfig(tc.args.data)
			if tc.want.wantErr {
				if err == nil {
					t.Errorf("ParseConfig(...): want error, got none\n%s", tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseConfig(...): unexpected error: %v\n%s", err, tc.reason)
			}
			if diff := cmp.Diff(tc.want.cfg, got); diff != "" {
				t.Errorf("ParseConfig(...): -want, +got:\n%s\n%s", diff, tc.reason)
			}
		})
	}
}

func TestConfigActive(t *testing.T) {
	cfg := Config{Count: 4, Draining: map[string]bool{"shard-3": true}}

	cases := map[string]struct {
		reason string
		shard  string
		want   bool
	}{
		"InRange":       {reason: "A shard inside the count and not draining is active.", shard: "shard-0", want: true},
		"LastInRange":   {reason: "shard-{count-1} is the highest active index.", shard: "shard-2", want: true},
		"Draining":      {reason: "A draining shard takes no new Workspaces.", shard: "shard-3", want: false},
		"OutOfRange":    {reason: "A shard at or above the count no longer exists.", shard: "shard-4", want: false},
		"Unlabelled":    {reason: "The empty label always needs placement.", shard: "", want: false},
		"Malformed":     {reason: "A hand-written label is treated as unplaced rather than trusted.", shard: "shard-1-old", want: false},
		"NoPrefix":      {reason: "A name without the shard- prefix is not a shard.", shard: "1", want: false},
		"LeadingZero":   {reason: "shard-01 is not canonical and must not alias shard-1.", shard: "shard-01", want: false},
		"NegativeIndex": {reason: "A negative index is not a shard.", shard: "shard--1", want: false},
		"EmptyIndex":    {reason: "A bare prefix is not a shard.", shard: "shard-", want: false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := cfg.Active(tc.shard); got != tc.want {
				t.Errorf("Active(%q): want %v, got %v\n%s", tc.shard, tc.want, got, tc.reason)
			}
		})
	}
}

func TestActiveShards(t *testing.T) {
	cases := map[string]struct {
		reason string
		cfg    Config
		want   []string
	}{
		"AllActive": {
			reason: "With nothing draining every shard is placeable, in index order.",
			cfg:    Config{Count: 3},
			want:   []string{"shard-0", "shard-1", "shard-2"},
		},
		"OneDraining": {
			reason: "A draining shard drops out but the rest keep their order.",
			cfg:    Config{Count: 4, Draining: map[string]bool{"shard-1": true}},
			want:   []string{"shard-0", "shard-2", "shard-3"},
		},
		"AllDraining": {
			reason: "Draining everything leaves nowhere to place, which callers must handle.",
			cfg:    Config{Count: 2, Draining: map[string]bool{"shard-0": true, "shard-1": true}},
			want:   []string{},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, tc.cfg.ActiveShards()); diff != "" {
				t.Errorf("ActiveShards(): -want, +got:\n%s\n%s", diff, tc.reason)
			}
		})
	}
}
