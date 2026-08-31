# Multi-Org GitHub App Credentials — Rollout Runbook

The terraform provider can clone modules from any GitHub org: it iterates
GitHub App secrets labeled `tf.konstruct.io/github-app: "true"` in
`crossplane-system`, mints an installation token from each (cached for
45 minutes), and uses the first token that can access the module's repository.
Install the App in an org, create one labeled secret, and Crossplane has
access — no ProviderConfig or Workspace changes.

- **Image:** `ghcr.io/konstructio/provider-terraform:branch-b42f23ce`
- **Change:** [konstructio/provider-terraform#11](https://github.com/konstructio/provider-terraform/pull/11)

## 1. Point the provider at the new image

`root-gitops → registry/konstruct-clusters/<cluster-name>/crossplane-components/deployment-runtime-config.yaml`

In the `package-runtime` container of the `DeploymentRuntimeConfig`, set:

```yaml
containers:
  - name: package-runtime
    image: ghcr.io/konstructio/provider-terraform:branch-b42f23ce
```

Leave the `Provider` package reference as it is — only the controller image
changes.

## 2. Update the log-streamer service selector

`same folder → svc.yaml`

The service selects pods by provider revision, and the revision hash changes
with the provider deployment:

```yaml
selector:
  pkg.crossplane.io/provider: provider-terraform
  pkg.crossplane.io/revision: crossplane-provider-terraform-f3bc0abf450b
```

> **If logs stop streaming after a later provider upgrade:** the revision hash
> has moved again. Get the current one with `kubectl get providerrevision` (or
> from the pod's `pkg.crossplane.io/revision` label) and update this selector.

## 3. Create one labeled secret per GitHub org

`same folder → crossplane-secrets.yaml`

Each secret represents one App installation. The label
`tf.konstruct.io/github-app: "true"` on the *target secret template* is what
the provider discovers. The example below gives Crossplane access to the
platform gitops repo (platform org) and the app gitops repo (application org):

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: github-app-credentials-1
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  refreshInterval: 120s
  secretStoreRef:
    kind: ClusterSecretStore
    name: e2e-aws-mgmt-3c7-vault-kv-secret
  target:
    name: github-app-credentials-1
    creationPolicy: Owner
    template:
      metadata:
        annotations:
          managed-by: argocd.argoproj.io
        labels:
          tf.konstruct.io/github-app: "true"
      type: Opaque
      data:
        # Template will be populated from Vault data
        type: "{{ .type }}"
        url: "{{ .url }}"
        app_id: "{{ .githubAppID }}"
        installation_id: "{{ .githubAppInstallationID }}"
        github_app_private_key: "{{ .githubAppPrivateKey }}"
  dataFrom:
    - extract:
        key: argocd/repo-credentials-template/e2e-cris-org-platform
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: github-app-credentials-2
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  refreshInterval: 120s
  secretStoreRef:
    kind: ClusterSecretStore
    name: e2e-aws-mgmt-3c7-vault-kv-secret
  target:
    name: github-app-credentials-2
    creationPolicy: Owner
    template:
      metadata:
        annotations:
          managed-by: argocd.argoproj.io
        labels:
          tf.konstruct.io/github-app: "true"
      type: Opaque
      data:
        # Template will be populated from Vault data
        type: "{{ .type }}"
        url: "{{ .url }}"
        app_id: "{{ .githubAppID }}"
        installation_id: "{{ .githubAppInstallationID }}"
        github_app_private_key: "{{ .githubAppPrivateKey }}"
  dataFrom:
    - extract:
        key: argocd/repo-credentials-template/e2e-cris-org
```

**To grant access to a new org:** install the GitHub App in that org, put its
credentials in Vault, and add one more `ExternalSecret` in this format
pointing at the new Vault key. That's the whole procedure.

| Secret key               | Value                                                                                                                                                                      |
| ------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `app_id`                 | Same for every installation of the same App.                                                                                                                                |
| `github_app_private_key` | Same for every installation of the same App (the App's one signing key).                                                                                                    |
| `installation_id`        | **Differs per org.** Org Settings → GitHub Apps → Configure — the number at the end of the URL. If two secrets carry the same `installation_id`, the second org has no access. |

> **Notes:** the provider only reads `app_id`, `installation_id`, and
> `github_app_private_key` — the `type`/`url` keys are ignored. The legacy
> unlabeled `github-app-credentials` secret keeps working as before. The label
> is a trust boundary: anyone who can create labeled secrets in
> `crossplane-system` controls what the provider clones with.

## How the provider resolves credentials

1. List secrets in `crossplane-system` labeled `tf.konstruct.io/github-app=true`
   (plus the legacy `github-app-credentials`), sorted by name.
2. For each, mint an installation token — or reuse the cached one if it's
   under 45 minutes old and the secret hasn't changed (tokens expire at
   1 hour; editing a secret re-mints immediately).
3. If the Workspace's module URL points at `github.com`, check the token can
   access that repository (`GET /repos/{org}/{repo}` — GitHub answers 404 when
   it can't). No access → try the next secret.
4. First working token becomes the git credentials for that Workspace's clone.
5. No secret works → the Workspace's `Synced` condition says exactly why each
   candidate failed (e.g. *"token cannot access repository org/repo: check the
   installation's org and repository access"*).
6. Only when *no* App secrets exist at all does the provider fall back to the
   ProviderConfig's own credential source (GitLab installs). When App secrets
   exist, they are authoritative.

## Verify

```sh
# provider pod picked up the new image and started cleanly
kubectl -n crossplane-system get pods -l pkg.crossplane.io/provider=provider-terraform \
  -o jsonpath='{.items[*].spec.containers[0].image}'

# both labeled secrets exist
kubectl -n crossplane-system get secrets -l tf.konstruct.io/github-app=true

# a Workspace from each org reconciles
kubectl get workspaces.tf.upbound.io
# expect SYNCED=True; on failure, `kubectl describe workspace <name>`
# shows which secret failed and why on the Synced condition
```

## Rollback

Revert the image in `deployment-runtime-config.yaml` to the previous tag and
revert the `svc.yaml` selector in the same commit. The labeled secrets are
ignored by the old image (it only reads the legacy `github-app-credentials`),
so they can stay.
