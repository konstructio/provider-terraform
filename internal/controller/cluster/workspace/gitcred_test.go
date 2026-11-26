package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	"github.com/crossplane/crossplane-runtime/v2/pkg/test"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/spf13/afero"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/upbound/provider-terraform/apis/cluster/v1beta1"
	tfClient "github.com/upbound/provider-terraform/internal/clients"
	"github.com/upbound/provider-terraform/internal/gitcred"
	"github.com/upbound/provider-terraform/internal/terraform"
)

// TestConnectGitCredDirPerCommand verifies that a Workspace's git credentials
// reach terraform through the command's own environment, not through the
// process environment that concurrent Workspaces share.
func TestConnectGitCredDirPerCommand(t *testing.T) {
	uid := types.UID("git-cred-uid")
	credDir := filepath.Join("/tmp", tfDir, string(uid))

	cases := map[string]struct {
		reason   string
		gitCreds bool
		env      []v1beta1.EnvVar
		want     []string
	}{
		"GitCredentials": {
			reason:   "terraform should get GIT_CRED_DIR pointing at this Workspace's credentials",
			gitCreds: true,
			want:     []string{gitcred.Env(credDir)},
		},
		"SpecEnvOverrides": {
			reason:   "an explicit GIT_CRED_DIR in the Workspace spec should come last so it wins",
			gitCreds: true,
			env:      []v1beta1.EnvVar{{Name: gitcred.EnvVar, Value: "/custom"}},
			want:     []string{gitcred.Env(credDir), gitcred.EnvVar + "=/custom"},
		},
		"NoGitCredentials": {
			reason: "terraform should not get GIT_CRED_DIR when the ProviderConfig has no git credentials",
			want:   []string{},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Fail if Connect leaks the directory into the process environment.
			t.Setenv(gitcred.EnvVar, "")
			if err := os.Unsetenv(gitcred.EnvVar); err != nil {
				t.Fatal(err)
			}

			var got []string
			c := connector{
				kube: &test.MockClient{
					MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
						if pc, ok := obj.(*v1beta1.ProviderConfig); ok && tc.gitCreds {
							pc.Spec.Credentials = []v1beta1.ProviderCredentials{{
								Filename: gitCredentialsFilename,
								Source:   xpv2.CredentialsSourceNone,
							}}
						}
						return nil
					}),
				},
				usage: tfClient.LegacyTrackerFn(func(_ context.Context, _ resource.LegacyManaged) error { return nil }),
				fs:    afero.Afero{Fs: afero.NewMemMapFs()},
				terraform: func(_ string, _ bool, _ bool, _ logging.Logger, envs ...string) tfclient {
					got = envs
					return &MockTf{
						MockInit:      func(_ context.Context, _ ...terraform.InitOption) error { return nil },
						MockWorkspace: func(_ context.Context, _ string) error { return nil },
					}
				},
				logger: logging.NewNopLogger(),
				gitCreds: func(context.Context, client.Client, string) ([]byte, error) {
					return []byte("https://x-access-token:token@github.com"), nil
				},
			}

			_, err := c.Connect(context.Background(), &v1beta1.Workspace{
				ObjectMeta: metav1.ObjectMeta{UID: uid},
				Spec: v1beta1.WorkspaceSpec{
					ClusterManagedResourceSpec: xpv2.ClusterManagedResourceSpec{
						ProviderConfigReference: &xpv2.Reference{},
					},
					ForProvider: v1beta1.WorkspaceParameters{
						Module: "module {}",
						Source: v1beta1.ModuleSourceInline,
						Env:    tc.env,
					},
				},
			})
			if err != nil {
				t.Fatalf("\n%s\nc.Connect(...): %v", tc.reason, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("\n%s\nterraform envs: -want, +got:\n%s", tc.reason, diff)
			}
			if v, ok := os.LookupEnv(gitcred.EnvVar); ok {
				t.Errorf("\n%s\nConnect set %s=%q in the process environment", tc.reason, gitcred.EnvVar, v)
			}
		})
	}
}
