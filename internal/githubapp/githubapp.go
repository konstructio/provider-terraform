// Package githubapp resolves git credentials from GitHub App installations.
//
// Credentials are discovered from Secrets in crossplane-system labeled
// tf.konstruct.io/github-app=true (plus the legacy unlabeled
// github-app-credentials secret). Each secret holds one installation
// (app_id, installation_id, github_app_private_key); the same App installed in
// several orgs is several secrets sharing app_id and key with distinct
// installation_ids. For each secret a token is minted and, when the module URL
// points at github.com, checked against the repository; the first token that
// works is returned as git credentials.
package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/upbound/provider-terraform/pkg/metrics"
)

const (
	secretNamespace  = "crossplane-system"
	secretLabel      = "tf.konstruct.io/github-app"
	legacySecretName = "github-app-credentials"
)

// ErrNoSecrets reports that no GitHub App secrets exist at all. It is the only
// error on which callers should fall back to the ProviderConfig credential
// source: when App secrets exist they are authoritative.
var ErrNoSecrets = errors.New("no github app secrets found")

// apiBaseURL is a variable so tests can point it at a stub server.
var apiBaseURL = "https://api.github.com"

// installationTokenResponse represents the GitHub API response for installation token creation.
type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// GetGitCredsFromGithubAppSecrets iterates the GitHub App secrets, mints an
// installation token from each, and returns git credentials from the first
// token that can access the repository referenced by module. Secrets whose
// token cannot access the repository (the App is not installed on that org, or
// the repo is not granted to the installation) are skipped. When module does
// not point at github.com the first token that mints successfully wins.
func GetGitCredsFromGithubAppSecrets(ctx context.Context, kube client.Client, module string) ([]byte, error) {
	secrets, err := discoverSecrets(ctx, kube)
	if err != nil {
		return nil, err
	}
	if len(secrets) == 0 {
		return nil, fmt.Errorf("%w in namespace %q (label %q or legacy %q)", ErrNoSecrets, secretNamespace, secretLabel, legacySecretName)
	}

	org, repo := ParseGitHubRepo(module)

	var failures []string
	for _, s := range secrets {
		token, err := mintFromSecret(ctx, s)
		if err != nil {
			failures = append(failures, fmt.Sprintf("secret %s: %v", s.Name, err))
			continue
		}
		if org != "" {
			if err := checkRepoAccess(ctx, token, org, repo); err != nil {
				failures = append(failures, fmt.Sprintf("secret %s: %v", s.Name, err))
				continue
			}
		}
		return fmt.Appendf(nil, "https://x-access-token:%s@github.com", token), nil
	}
	return nil, fmt.Errorf("no github app secret works for module %q: %s", module, strings.Join(failures, "; "))
}

func discoverSecrets(ctx context.Context, kube client.Client) ([]v1.Secret, error) {
	list := &v1.SecretList{}
	if err := kube.List(ctx, list, client.InNamespace(secretNamespace), client.MatchingLabels{secretLabel: "true"}); err != nil {
		return nil, fmt.Errorf("failed to list github app secrets: %w", err)
	}
	secrets := list.Items
	seen := map[string]bool{}
	for _, s := range secrets {
		seen[s.Name] = true
	}
	if !seen[legacySecretName] {
		legacy := &v1.Secret{}
		if err := kube.Get(ctx, types.NamespacedName{Namespace: secretNamespace, Name: legacySecretName}, legacy); err == nil {
			secrets = append(secrets, *legacy)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })
	return secrets, nil
}

func mintFromSecret(ctx context.Context, secret v1.Secret) (string, error) {
	privateKeyPEM := string(secret.Data["github_app_private_key"])

	appID, err := strconv.ParseInt(string(secret.Data["app_id"]), 10, 64)
	if err != nil {
		return "", fmt.Errorf("failed to parse app_id from secret: %w", err)
	}

	installationID, err := strconv.ParseInt(string(secret.Data["installation_id"]), 10, 64)
	if err != nil {
		return "", fmt.Errorf("failed to parse installation_id from secret: %w", err)
	}

	token, err := generateInstallationToken(ctx, appID, installationID, privateKeyPEM)
	if err != nil {
		return "", fmt.Errorf("failed to get installation token: %w", err)
	}
	return token, nil
}

// checkRepoAccess verifies that token can see org/repo, so a scope problem
// surfaces as a clear Connect error instead of an opaque clone failure. GitHub
// answers 404 (not 403) when the token has no access. A transport error is
// non-fatal: the clone gets to decide.
func checkRepoAccess(ctx context.Context, token, org, repo string) error {
	url := fmt.Sprintf("%s/repos/%s/%s", apiBaseURL, org, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("error creating repo access request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	apiStart := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		metrics.RecordGitHubAPICall(org, "check_repo_access", "failure", time.Since(apiStart))
		return nil //nolint:nilerr // deliberate: the check is best-effort
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		metrics.RecordGitHubAPICall(org, "check_repo_access", "success", time.Since(apiStart))
		return nil
	case resp.StatusCode == http.StatusNotFound:
		metrics.RecordGitHubAPICall(org, "check_repo_access", "failure", time.Since(apiStart))
		return fmt.Errorf("token cannot access repository %s/%s: check the installation's org and repository access", org, repo)
	default:
		metrics.RecordGitHubAPICall(org, "check_repo_access", "failure", time.Since(apiStart))
		return fmt.Errorf("repository access check for %s/%s returned status %d", org, repo, resp.StatusCode)
	}
}

func generateInstallationToken(ctx context.Context, appID, installationID int64, privateKeyPEM string) (string, error) {
	// Parse the RSA private key from PEM
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))
	if err != nil {
		return "", fmt.Errorf("failed to parse GitHub App private key: %w", err)
	}

	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)), // Clock drift allowance
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),  // Max 10 min per GitHub docs
		Issuer:    fmt.Sprintf("%d", appID),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signedJWT, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("failed to sign JWT: %w", err)
	}

	// Exchange JWT for installation access token
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", apiBaseURL, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", fmt.Errorf("error creating installation token request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", signedJWT))
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	apiStart := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		metrics.RecordGitHubAPICall("unknown", "generate_installation_token", "failure", time.Since(apiStart))
		return "", fmt.Errorf("error requesting installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		metrics.RecordGitHubAPICall("unknown", "generate_installation_token", "failure", time.Since(apiStart))
		return "", fmt.Errorf("error reading installation token response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		metrics.RecordGitHubAPICall("unknown", "generate_installation_token", "failure", time.Since(apiStart))
		return "", fmt.Errorf("failed to create installation token (status %d): %s", resp.StatusCode, string(body))
	}

	metrics.RecordGitHubAPICall("unknown", "generate_installation_token", "success", time.Since(apiStart))

	var tokenResp installationTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("error unmarshaling installation token response: %w", err)
	}

	return tokenResp.Token, nil
}

// githubRepoPattern extracts the org and repository from a module source that
// points at github.com, e.g.
// "git::https://github.com/org/repo.git//modules/x?ref=main".
var githubRepoPattern = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/?#]+)`)

// ParseGitHubRepo returns the org and repository name from a module URL, or
// empty strings when module does not reference github.com.
func ParseGitHubRepo(module string) (org, repo string) {
	m := githubRepoPattern.FindStringSubmatch(module)
	if len(m) < 3 {
		return "", ""
	}
	return m[1], strings.TrimSuffix(m[2], ".git")
}
