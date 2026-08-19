package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/upbound/provider-terraform/pkg/metrics"
)

// installationTokenResponse represents the GitHub API response for installation token creation.
type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func getGitCredsFromGithubAppSecret(ctx context.Context, client client.Client) ([]byte, error) {
	secret := v1.Secret{}
	if err := client.Get(ctx, types.NamespacedName{
		Name:      "github-app-credentials",
		Namespace: "crossplane-system",
	}, &secret); err != nil {
		return nil, fmt.Errorf("failed to get github app credentials secret: %w", err)
	}

	privateKeyPEM := string(secret.Data["github_app_private_key"])

	appIDStr := string(secret.Data["app_id"])
	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app_id from secret: %w", err)
	}

	installIDStr := string(secret.Data["installation_id"])
	installationID, err := strconv.ParseInt(installIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse installation_id from secret: %w", err)
	}

	installationToken, err := generateInstallationToken(appID, installationID, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation token: %w", err)
	}

	data := fmt.Sprintf("https://x-access-token:%s@github.com", installationToken)

	return []byte(data), nil
}

func generateInstallationToken(appID, installationID int64, privateKeyPEM string) (string, error) {
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
	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID)
	req, err := http.NewRequest(http.MethodPost, url, nil)
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
	defer resp.Body.Close()

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
