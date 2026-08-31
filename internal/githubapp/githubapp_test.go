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

package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func appSecret(name, appID, installationID, key string) *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: secretNamespace,
			Labels:    map[string]string{secretLabel: "true"},
		},
		Data: map[string][]byte{
			"app_id":                 []byte(appID),
			"installation_id":        []byte(installationID),
			"github_app_private_key": []byte(key),
		},
	}
}

// fakeGitHub serves the two endpoints used: token minting per installation id,
// and the repo access check. repos maps "org/repo" to the installation id
// whose token may access it.
type fakeGitHub struct {
	installations map[string]bool
	repos         map[string]string
	mints         int
}

func (g *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !g.installations[id] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		g.mints++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_%s","expires_at":%q}`, id, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{org}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		id := g.repos[r.PathValue("org")+"/"+r.PathValue("repo")]
		if id != "" && r.Header.Get("Authorization") == "Bearer ghs_"+id {
			fmt.Fprint(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

func stubGitHub(t *testing.T, gh *fakeGitHub) {
	t.Helper()
	srv := httptest.NewServer(gh.handler())
	prev := apiBaseURL
	apiBaseURL = srv.URL
	tokenCache = map[string]cachedToken{}
	t.Cleanup(func() { apiBaseURL = prev; srv.Close(); tokenCache = map[string]cachedToken{} })
}

func fakeKube(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithObjects(objs...).Build()
}

func TestGetGitCredsFromGithubAppSecrets(t *testing.T) {
	key := testPrivateKeyPEM(t)
	ctx := context.Background()

	t.Run("FirstSecretWithRepoAccessWins", func(t *testing.T) {
		stubGitHub(t, &fakeGitHub{
			installations: map[string]bool{"111": true, "222": true},
			repos:         map[string]string{"gitops-ing/gitops": "222"},
		})
		kube := fakeKube(t,
			appSecret("gh-konstructio", "42", "111", key),
			appSecret("gh-gitops-ing", "42", "222", key),
		)

		data, err := GetGitCredsFromGithubAppSecrets(ctx, kube, "git::https://github.com/gitops-ing/gitops.git//terraform/aws?ref=main")
		if err != nil {
			t.Fatalf("GetGitCredsFromGithubAppSecrets(...): %v", err)
		}
		if want := "https://x-access-token:ghs_222@github.com"; string(data) != want {
			t.Errorf("got %q, want %q", data, want)
		}
	})

	t.Run("NoGitHubModuleReturnsFirstMintedToken", func(t *testing.T) {
		stubGitHub(t, &fakeGitHub{installations: map[string]bool{"111": true}})
		kube := fakeKube(t, appSecret("gh", "42", "111", key))

		data, err := GetGitCredsFromGithubAppSecrets(ctx, kube, "inline HCL, no github url")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "ghs_111") {
			t.Errorf("want first minted token, got %q", data)
		}
	})

	t.Run("NoSecretWithAccessFails", func(t *testing.T) {
		stubGitHub(t, &fakeGitHub{installations: map[string]bool{"111": true}})
		kube := fakeKube(t, appSecret("gh", "42", "111", key))

		_, err := GetGitCredsFromGithubAppSecrets(ctx, kube, "git::https://github.com/other-org/hidden.git")
		if err == nil || !strings.Contains(err.Error(), "cannot access repository other-org/hidden") {
			t.Errorf("want repo access error, got: %v", err)
		}
	})

	t.Run("BadSecretSkippedGoodSecretWins", func(t *testing.T) {
		stubGitHub(t, &fakeGitHub{
			installations: map[string]bool{"111": true},
			repos:         map[string]string{"konstructio/infra": "111"},
		})
		kube := fakeKube(t,
			appSecret("aaa-bad", "not-a-number", "111", key),
			appSecret("gh", "42", "111", key),
		)

		if _, err := GetGitCredsFromGithubAppSecrets(ctx, kube, "git::https://github.com/konstructio/infra.git"); err != nil {
			t.Fatalf("good secret should win despite bad sibling: %v", err)
		}
	})

	t.Run("TokenIsCachedFor45Minutes", func(t *testing.T) {
		gh := &fakeGitHub{
			installations: map[string]bool{"111": true},
			repos:         map[string]string{"konstructio/infra": "111"},
		}
		stubGitHub(t, gh)
		kube := fakeKube(t, appSecret("gh", "42", "111", key))

		for i := 0; i < 3; i++ {
			if _, err := GetGitCredsFromGithubAppSecrets(ctx, kube, "git::https://github.com/konstructio/infra.git"); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		if gh.mints != 1 {
			t.Errorf("want 1 mint across repeat calls, got %d", gh.mints)
		}

		// An expired cache entry is re-minted.
		tokenCacheMu.Lock()
		tokenCache["gh"] = cachedToken{token: tokenCache["gh"].token, issuedAt: time.Now().Add(-46 * time.Minute), secretRV: tokenCache["gh"].secretRV}
		tokenCacheMu.Unlock()
		if _, err := GetGitCredsFromGithubAppSecrets(ctx, kube, "git::https://github.com/konstructio/infra.git"); err != nil {
			t.Fatal(err)
		}
		if gh.mints != 2 {
			t.Errorf("want re-mint after 45m, got %d mints", gh.mints)
		}
	})

	t.Run("RotatedSecretRemints", func(t *testing.T) {
		gh := &fakeGitHub{installations: map[string]bool{"111": true}}
		stubGitHub(t, gh)

		sec := appSecret("gh", "42", "111", key)
		sec.ResourceVersion = "1"
		if _, err := GetGitCredsFromGithubAppSecrets(ctx, fakeKube(t, sec), "inline"); err != nil {
			t.Fatal(err)
		}
		sec2 := appSecret("gh", "42", "111", key)
		sec2.ResourceVersion = "2"
		if _, err := GetGitCredsFromGithubAppSecrets(ctx, fakeKube(t, sec2), "inline"); err != nil {
			t.Fatal(err)
		}
		if gh.mints != 2 {
			t.Errorf("want re-mint after secret rotation, got %d mints", gh.mints)
		}
	})

	t.Run("NoSecretsIsSentinel", func(t *testing.T) {
		stubGitHub(t, &fakeGitHub{})
		_, err := GetGitCredsFromGithubAppSecrets(ctx, fakeKube(t), "git::https://github.com/o/r.git")
		if !errors.Is(err, ErrNoSecrets) {
			t.Errorf("want ErrNoSecrets, got: %v", err)
		}
	})

	t.Run("LegacyUnlabeledSecretHonored", func(t *testing.T) {
		stubGitHub(t, &fakeGitHub{installations: map[string]bool{"111": true}})
		legacy := appSecret(legacySecretName, "42", "111", key)
		legacy.Labels = nil

		data, err := GetGitCredsFromGithubAppSecrets(ctx, fakeKube(t, legacy), "inline module")
		if err != nil {
			t.Fatalf("legacy secret should be discovered: %v", err)
		}
		if !strings.Contains(string(data), "ghs_111") {
			t.Errorf("want legacy secret token, got %q", data)
		}
	})
}

func TestParseGitHubRepo(t *testing.T) {
	cases := map[string]struct{ module, org, repo string }{
		"GoGetterWithSubdirAndRef": {"git::https://github.com/konstructio/konstruct-templates.git//terraform/aws/modules/x?ref=main", "konstructio", "konstruct-templates"},
		"PlainHTTPS":               {"https://github.com/org/repo", "org", "repo"},
		"NoScheme":                 {"github.com/org/repo//sub", "org", "repo"},
		"SSHStyle":                 {"git@github.com:org/repo.git", "org", "repo"},
		"NotGitHub":                {"git::https://gitlab.com/org/repo.git", "", ""},
		"InlineHCL":                {"resource \"null_resource\" \"x\" {}", "", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			org, repo := ParseGitHubRepo(tc.module)
			if org != tc.org || repo != tc.repo {
				t.Errorf("ParseGitHubRepo(%q) = %q,%q want %q,%q", tc.module, org, repo, tc.org, tc.repo)
			}
		})
	}
}
