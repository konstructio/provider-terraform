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

func appSecret(name, rv, appID, installationID, key string) *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       DefaultSecretNamespace,
			ResourceVersion: rv,
			Labels:          map[string]string{DefaultSecretLabel: "true"},
		},
		Data: map[string][]byte{
			"app_id":                 []byte(appID),
			"installation_id":        []byte(installationID),
			"github_app_private_key": []byte(key),
		},
	}
}

// fakeGitHub serves the three endpoints the manager uses. Installations map
// installation id -> org; repos holds "org/repo" keys the tokens may access.
type fakeGitHub struct {
	installations map[string]string
	repos         map[string]bool
	mints         int
}

func (g *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := g.installations[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		g.mints++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_%s","expires_at":%q}`, id, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /app/installations/{id}", func(w http.ResponseWriter, r *http.Request) {
		org, ok := g.installations[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"account":{"login":%q}}`, org)
	})
	mux.HandleFunc("GET /repos/{org}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if g.repos[r.PathValue("org")+"/"+r.PathValue("repo")] {
			fmt.Fprint(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

func newTestManager(t *testing.T, gh *fakeGitHub) *Manager {
	t.Helper()
	srv := httptest.NewServer(gh.handler())
	t.Cleanup(srv.Close)
	m := NewManager()
	m.apiBase = srv.URL
	m.httpClient = srv.Client()
	return m
}

func fakeKube(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithObjects(objs...).Build()
}

func TestGitCredentials(t *testing.T) {
	key := testPrivateKeyPEM(t)
	ctx := context.Background()

	t.Run("MultiOrgWritesPathScopedLines", func(t *testing.T) {
		gh := &fakeGitHub{
			installations: map[string]string{"111": "konstructio", "222": "gitops-ing"},
			repos:         map[string]bool{"konstructio/konstruct-templates": true},
		}
		m := newTestManager(t, gh)
		kube := fakeKube(t,
			appSecret("gh-konstructio", "1", "42", "111", key),
			appSecret("gh-gitops-ing", "1", "42", "222", key),
		)

		module := "git::https://github.com/konstructio/konstruct-templates.git//terraform/aws?ref=main"
		b, err := m.GitCredentials(ctx, kube, module)
		if err != nil {
			t.Fatalf("GitCredentials(...): %v", err)
		}
		got := string(b)
		for _, want := range []string{
			"https://x-access-token:ghs_222@github.com/gitops-ing\n",
			"https://x-access-token:ghs_111@github.com/konstructio\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("credentials file missing %q, got:\n%s", want, got)
			}
		}
	})

	t.Run("TokenAndProbeAreCached", func(t *testing.T) {
		gh := &fakeGitHub{
			installations: map[string]string{"111": "konstructio"},
			repos:         map[string]bool{"konstructio/infra": true},
		}
		m := newTestManager(t, gh)
		kube := fakeKube(t, appSecret("gh", "1", "42", "111", key))

		module := "git::https://github.com/konstructio/infra.git?ref=v1"
		for i := 0; i < 3; i++ {
			if _, err := m.GitCredentials(ctx, kube, module); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		if gh.mints != 1 {
			t.Errorf("want 1 token mint across repeat calls, got %d", gh.mints)
		}
	})

	t.Run("RotatedSecretRemints", func(t *testing.T) {
		gh := &fakeGitHub{installations: map[string]string{"111": "konstructio"}}
		m := newTestManager(t, gh)

		if _, err := m.GitCredentials(ctx, fakeKube(t, appSecret("gh", "1", "42", "111", key)), "inline"); err != nil {
			t.Fatal(err)
		}
		if _, err := m.GitCredentials(ctx, fakeKube(t, appSecret("gh", "2", "42", "111", key)), "inline"); err != nil {
			t.Fatal(err)
		}
		if gh.mints != 2 {
			t.Errorf("want re-mint after secret rotation, got %d mints", gh.mints)
		}
	})

	t.Run("RepoNotGrantedFails", func(t *testing.T) {
		gh := &fakeGitHub{installations: map[string]string{"111": "konstructio"}}
		m := newTestManager(t, gh)
		kube := fakeKube(t, appSecret("gh", "1", "42", "111", key))

		_, err := m.GitCredentials(ctx, kube, "git::https://github.com/konstructio/hidden.git")
		if err == nil || !strings.Contains(err.Error(), "cannot access repository") {
			t.Errorf("want repository access error, got: %v", err)
		}
	})

	t.Run("OrgWithoutInstallationFails", func(t *testing.T) {
		gh := &fakeGitHub{installations: map[string]string{"111": "konstructio"}}
		m := newTestManager(t, gh)
		kube := fakeKube(t, appSecret("gh", "1", "42", "111", key))

		_, err := m.GitCredentials(ctx, kube, "git::https://github.com/other-org/repo.git")
		if err == nil || !strings.Contains(err.Error(), `no github app installation covers org "other-org"`) {
			t.Errorf("want no-installation error, got: %v", err)
		}
	})

	t.Run("NoSecretsIsSentinel", func(t *testing.T) {
		m := newTestManager(t, &fakeGitHub{})
		_, err := m.GitCredentials(ctx, fakeKube(t), "git::https://github.com/o/r.git")
		if !errors.Is(err, ErrNoSecrets) {
			t.Errorf("want ErrNoSecrets, got: %v", err)
		}
	})

	t.Run("BadSecretSkippedGoodSecretWins", func(t *testing.T) {
		gh := &fakeGitHub{
			installations: map[string]string{"111": "konstructio"},
			repos:         map[string]bool{"konstructio/infra": true},
		}
		m := newTestManager(t, gh)
		bad := appSecret("aaa-bad", "1", "not-a-number", "111", key)
		kube := fakeKube(t, bad, appSecret("gh", "1", "42", "111", key))

		if _, err := m.GitCredentials(ctx, kube, "git::https://github.com/konstructio/infra.git"); err != nil {
			t.Fatalf("good secret should win despite bad sibling: %v", err)
		}
	})

	t.Run("LegacyUnlabeledSecretHonored", func(t *testing.T) {
		gh := &fakeGitHub{installations: map[string]string{"111": "konstructio"}}
		m := newTestManager(t, gh)
		legacy := appSecret(legacySecretName, "1", "42", "111", key)
		legacy.Labels = nil
		kube := fakeKube(t, legacy)

		b, err := m.GitCredentials(ctx, kube, "inline module, no github url")
		if err != nil {
			t.Fatalf("legacy secret should be discovered: %v", err)
		}
		if !strings.Contains(string(b), "github.com/konstructio") {
			t.Errorf("want konstructio line, got: %s", b)
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
