// Package githubapp resolves git credentials for remote Terraform modules from
// GitHub App installations.
//
// GitHub App credentials are discovered from Secrets in the configured
// namespace carrying the configured label (one Secret per installation; the
// same App installed in several orgs is several Secrets sharing app_id and
// private key but with distinct installation_ids). For every discovered Secret
// the manager mints an installation token, resolves which org the installation
// belongs to, and writes one path-scoped line per org into a single shared git
// credentials file:
//
//	https://x-access-token:<token>@github.com/<org>
//
// Together with `useHttpPath = true` in the container's gitconfig, git picks
// the right token by repository path, so concurrent reconciles across orgs
// share one file and cannot race each other's credentials.
//
// When the Workspace's module URL points at github.com, the manager also
// probes the repository with the org's token (GET /repos/{org}/{repo}) so a
// "the App is not installed there / the repo is not granted" failure surfaces
// as a clear Connect error instead of an opaque clone failure.
package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/upbound/provider-terraform/pkg/metrics"
)

const (
	// DefaultSecretNamespace is where GitHub App secrets are discovered.
	DefaultSecretNamespace = "crossplane-system"
	// DefaultSecretLabel marks a Secret as GitHub App credentials.
	DefaultSecretLabel = "tf.konstruct.io/github-app"
	// legacySecretName is the pre-label secret honored for backward
	// compatibility even when it carries no label.
	legacySecretName = "github-app-credentials"

	// CredentialsDir holds the shared git credentials file. It must NOT live
	// under /tmp/tf, which the workdir garbage collector prunes.
	CredentialsDir = "/tmp/tf-github-app"
	// CredentialsFile is referenced by the container's gitconfig.
	CredentialsFile = ".git-credentials"

	tokenRefreshMargin = 5 * time.Minute
	probeTimeout       = 5 * time.Second
)

// installationTokenResponse represents the GitHub API response for
// installation token creation.
type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type tokenEntry struct {
	org       string
	token     string
	expiresAt time.Time
	secretRV  string
	// probed caches repositories this token has been verified against, so the
	// probe runs once per token lifetime rather than once per reconcile.
	probed map[string]bool
}

func (t *tokenEntry) live(now time.Time) bool {
	return t != nil && t.expiresAt.Sub(now) > tokenRefreshMargin
}

// Manager mints and caches GitHub App installation tokens for every labeled
// secret and maintains the shared git credentials file. It is safe for
// concurrent use.
type Manager struct {
	namespace string
	label     string

	mu     sync.Mutex
	tokens map[string]*tokenEntry // key: secret name

	// overridable for tests
	apiBase    string
	httpClient *http.Client
	credFile   string
	now        func() time.Time
}

// NewManager returns a Manager with production defaults. Namespace and label
// may be overridden with the GITHUB_APP_SECRET_NAMESPACE and
// GITHUB_APP_SECRET_LABEL environment variables.
func NewManager() *Manager {
	ns := os.Getenv("GITHUB_APP_SECRET_NAMESPACE")
	if ns == "" {
		ns = DefaultSecretNamespace
	}
	label := os.Getenv("GITHUB_APP_SECRET_LABEL")
	if label == "" {
		label = DefaultSecretLabel
	}
	return &Manager{
		namespace:  ns,
		label:      label,
		tokens:     map[string]*tokenEntry{},
		apiBase:    "https://api.github.com",
		httpClient: &http.Client{Timeout: 30 * time.Second},
		credFile:   filepath.Join(CredentialsDir, CredentialsFile),
		now:        time.Now,
	}
}

var (
	shared     *Manager
	sharedOnce sync.Once
)

// Shared returns the process-wide Manager used by the workspace controllers.
func Shared() *Manager {
	sharedOnce.Do(func() { shared = NewManager() })
	return shared
}

// EnsureCredentials makes sure every discoverable GitHub App installation has
// a live token in the shared credentials file, and, when module points at a
// github.com repository, verifies that one of those tokens can access it. It
// returns an error when no GitHub App credentials are usable for module; the
// caller is expected to fall back to the ProviderConfig's own credential
// source.
func (m *Manager) EnsureCredentials(ctx context.Context, kube client.Client, module string) error {
	secrets, err := m.discoverSecrets(ctx, kube)
	if err != nil {
		return err
	}
	if len(secrets) == 0 {
		return fmt.Errorf("no github app secrets found in namespace %q (label %q or legacy %q)", m.namespace, m.label, legacySecretName)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var failures []string
	for _, s := range secrets {
		if err := m.ensureToken(ctx, s); err != nil {
			failures = append(failures, fmt.Sprintf("secret %s: %v", s.Name, err))
		}
	}
	// Drop cache entries whose secret is gone.
	names := map[string]bool{}
	for _, s := range secrets {
		names[s.Name] = true
	}
	for name := range m.tokens {
		if !names[name] {
			delete(m.tokens, name)
		}
	}

	live := 0
	for _, t := range m.tokens {
		if t.live(m.now()) {
			live++
		}
	}
	if live == 0 {
		return fmt.Errorf("no usable github app token: %s", strings.Join(failures, "; "))
	}

	if err := m.writeCredentialsFile(); err != nil {
		return fmt.Errorf("cannot write git credentials file: %w", err)
	}

	// When we can tell which repository the module needs, verify access so a
	// scope problem surfaces here with a clear message instead of failing the
	// clone. Modules that don't point at github.com (or inline HCL whose
	// nested module sources we cannot see) are served by the file as a whole.
	if org, repo := ParseGitHubRepo(module); org != "" {
		return m.verifyRepoAccess(ctx, org, repo, failures)
	}
	return nil
}

func (m *Manager) discoverSecrets(ctx context.Context, kube client.Client) ([]v1.Secret, error) {
	list := &v1.SecretList{}
	if err := kube.List(ctx, list, client.InNamespace(m.namespace), client.MatchingLabels{m.label: "true"}); err != nil {
		return nil, fmt.Errorf("cannot list github app secrets: %w", err)
	}
	secrets := list.Items
	found := map[string]bool{}
	for _, s := range secrets {
		found[s.Name] = true
	}
	if !found[legacySecretName] {
		legacy := &v1.Secret{}
		if err := kube.Get(ctx, types.NamespacedName{Namespace: m.namespace, Name: legacySecretName}, legacy); err == nil {
			secrets = append(secrets, *legacy)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })
	return secrets, nil
}

// ensureToken mints (or reuses) the installation token for one secret and
// resolves the org it belongs to. Callers must hold m.mu.
func (m *Manager) ensureToken(ctx context.Context, s v1.Secret) error {
	if t := m.tokens[s.Name]; t.live(m.now()) && t.secretRV == s.ResourceVersion {
		return nil
	}

	appID, err := strconv.ParseInt(string(s.Data["app_id"]), 10, 64)
	if err != nil {
		return fmt.Errorf("cannot parse app_id: %w", err)
	}
	installationID, err := strconv.ParseInt(string(s.Data["installation_id"]), 10, 64)
	if err != nil {
		return fmt.Errorf("cannot parse installation_id: %w", err)
	}
	privateKeyPEM := string(s.Data["github_app_private_key"])

	appJWT, err := signAppJWT(appID, privateKeyPEM, m.now())
	if err != nil {
		return err
	}
	tok, expiresAt, err := m.mintInstallationToken(ctx, appJWT, installationID)
	if err != nil {
		return err
	}
	org, err := m.installationOrg(ctx, appJWT, installationID)
	if err != nil {
		return err
	}

	m.tokens[s.Name] = &tokenEntry{
		org:       org,
		token:     tok,
		expiresAt: expiresAt,
		secretRV:  s.ResourceVersion,
		probed:    map[string]bool{},
	}
	return nil
}

// verifyRepoAccess probes org/repo with the matching cached token. Callers
// must hold m.mu.
func (m *Manager) verifyRepoAccess(ctx context.Context, org, repo string, failures []string) error {
	var entry *tokenEntry
	for _, t := range m.tokens {
		if t.live(m.now()) && strings.EqualFold(t.org, org) {
			entry = t
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("no github app installation covers org %q (have: %s)%s", org, strings.Join(m.orgs(), ", "), suffix(failures))
	}
	key := org + "/" + repo
	if entry.probed[key] {
		return nil
	}

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s", m.apiBase, org, repo), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+entry.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	start := time.Now()
	resp, err := m.httpClient.Do(req)
	if err != nil {
		// A network blip should not block a reconcile whose token was minted
		// fine; let the clone be the judge.
		metrics.RecordGitHubAPICall(org, "probe_repo", "failure", time.Since(start))
		return nil //nolint:nilerr // deliberate: the probe is best-effort
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		metrics.RecordGitHubAPICall(org, "probe_repo", "success", time.Since(start))
		entry.probed[key] = true
		return nil
	case resp.StatusCode == http.StatusNotFound:
		metrics.RecordGitHubAPICall(org, "probe_repo", "failure", time.Since(start))
		return fmt.Errorf("github app installation for org %q cannot access repository %q: check the installation's repository access", org, key)
	default:
		metrics.RecordGitHubAPICall(org, "probe_repo", "failure", time.Since(start))
		return fmt.Errorf("github repository probe for %q returned status %d", key, resp.StatusCode)
	}
}

func (m *Manager) orgs() []string {
	orgs := make([]string, 0, len(m.tokens))
	for _, t := range m.tokens {
		if t.live(m.now()) {
			orgs = append(orgs, t.org)
		}
	}
	sort.Strings(orgs)
	return orgs
}

func suffix(failures []string) string {
	if len(failures) == 0 {
		return ""
	}
	return " (" + strings.Join(failures, "; ") + ")"
}

// writeCredentialsFile atomically rewrites the shared credentials file with
// one path-scoped line per live token. Callers must hold m.mu.
func (m *Manager) writeCredentialsFile() error {
	var b strings.Builder
	for _, name := range m.sortedTokenNames() {
		t := m.tokens[name]
		if !t.live(m.now()) {
			continue
		}
		fmt.Fprintf(&b, "https://x-access-token:%s@github.com/%s\n", t.token, t.org)
	}
	dir := filepath.Dir(m.credFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, CredentialsFile+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), m.credFile)
}

func (m *Manager) sortedTokenNames() []string {
	names := make([]string, 0, len(m.tokens))
	for n := range m.tokens {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func signAppJWT(appID int64, privateKeyPEM string, now time.Time) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))
	if err != nil {
		return "", fmt.Errorf("cannot parse GitHub App private key: %w", err)
	}
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)), // Clock drift allowance
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),  // Max 10 min per GitHub docs
		Issuer:    fmt.Sprintf("%d", appID),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("cannot sign GitHub App JWT: %w", err)
	}
	return signed, nil
}

func (m *Manager) mintInstallationToken(ctx context.Context, appJWT string, installationID int64) (string, time.Time, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", m.apiBase, installationID)
	body, status, err := m.appAPICall(ctx, http.MethodPost, url, appJWT, "generate_installation_token")
	if err != nil {
		return "", time.Time{}, err
	}
	if status != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("cannot create installation token (status %d): %s", status, string(body))
	}
	var tokenResp installationTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", time.Time{}, fmt.Errorf("cannot unmarshal installation token response: %w", err)
	}
	return tokenResp.Token, tokenResp.ExpiresAt, nil
}

func (m *Manager) installationOrg(ctx context.Context, appJWT string, installationID int64) (string, error) {
	url := fmt.Sprintf("%s/app/installations/%d", m.apiBase, installationID)
	body, status, err := m.appAPICall(ctx, http.MethodGet, url, appJWT, "get_installation")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("cannot get installation %d (status %d): %s", installationID, status, string(body))
	}
	var inst struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &inst); err != nil {
		return "", fmt.Errorf("cannot unmarshal installation response: %w", err)
	}
	if inst.Account.Login == "" {
		return "", fmt.Errorf("installation %d has no account login", installationID)
	}
	return inst.Account.Login, nil
}

func (m *Manager) appAPICall(ctx context.Context, method, url, appJWT, operation string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot create %s request: %w", operation, err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	start := time.Now()
	resp, err := m.httpClient.Do(req)
	if err != nil {
		metrics.RecordGitHubAPICall("unknown", operation, "failure", time.Since(start))
		return nil, 0, fmt.Errorf("%s request failed: %w", operation, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		metrics.RecordGitHubAPICall("unknown", operation, "failure", time.Since(start))
		return nil, 0, fmt.Errorf("cannot read %s response: %w", operation, err)
	}
	status := "success"
	if resp.StatusCode >= 400 {
		status = "failure"
	}
	metrics.RecordGitHubAPICall("unknown", operation, status, time.Since(start))
	return body, resp.StatusCode, nil
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
	repo = strings.TrimSuffix(m[2], ".git")
	return m[1], repo
}
