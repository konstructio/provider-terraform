/*
Copyright 2021 The Crossplane Authors.

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

package terraform

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/afero"
)

const testLock = `
provider "registry.terraform.io/hashicorp/kubernetes" {
  version     = "2.23.0"
  constraints = "2.23.0"
  hashes      = ["h1:abc"]
}

provider "registry.terraform.io/civo/civo" {
  version = "1.0.35"
  hashes  = ["h1:def"]
}
`

// writePlugin creates a non-empty plugin dir for source@version under dir.
func writePlugin(t *testing.T, fs afero.Fs, dir, source, version string) {
	t.Helper()
	p := filepath.Join(dir, ".terraform", "providers", filepath.FromSlash(source), version, runtime.GOOS+"_"+runtime.GOARCH)
	if err := fs.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := afero.WriteFile(fs, filepath.Join(p, "terraform-provider"), []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProvidersInstalled(t *testing.T) {
	const dir = "/tf/ws"
	cases := map[string]struct {
		setup func(t *testing.T, fs afero.Fs)
		want  bool
	}{
		"NoLockFile": {
			setup: func(t *testing.T, fs afero.Fs) {},
			want:  false,
		},
		"AllPluginsPresent": {
			setup: func(t *testing.T, fs afero.Fs) {
				if err := afero.WriteFile(fs, filepath.Join(dir, ".terraform.lock.hcl"), []byte(testLock), 0o644); err != nil {
					t.Fatal(err)
				}
				writePlugin(t, fs, dir, "registry.terraform.io/hashicorp/kubernetes", "2.23.0")
				writePlugin(t, fs, dir, "registry.terraform.io/civo/civo", "1.0.35")
			},
			want: true,
		},
		"OnePluginMissing": {
			setup: func(t *testing.T, fs afero.Fs) {
				if err := afero.WriteFile(fs, filepath.Join(dir, ".terraform.lock.hcl"), []byte(testLock), 0o644); err != nil {
					t.Fatal(err)
				}
				// Only install one of the two pinned providers; this is the
				// partial-.terraform case that used to be skipped past init.
				writePlugin(t, fs, dir, "registry.terraform.io/hashicorp/kubernetes", "2.23.0")
			},
			want: false,
		},
		"EmptyPluginDir": {
			setup: func(t *testing.T, fs afero.Fs) {
				if err := afero.WriteFile(fs, filepath.Join(dir, ".terraform.lock.hcl"), []byte(testLock), 0o644); err != nil {
					t.Fatal(err)
				}
				writePlugin(t, fs, dir, "registry.terraform.io/hashicorp/kubernetes", "2.23.0")
				// civo plugin dir exists but is empty.
				empty := filepath.Join(dir, ".terraform", "providers", "registry.terraform.io", "civo", "civo", "1.0.35", runtime.GOOS+"_"+runtime.GOARCH)
				if err := fs.MkdirAll(empty, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fs := afero.NewMemMapFs()
			tc.setup(t, fs)
			if got := ProvidersInstalled(fs, dir); got != tc.want {
				t.Errorf("ProvidersInstalled(%q) = %v, want %v", dir, got, tc.want)
			}
		})
	}
}
