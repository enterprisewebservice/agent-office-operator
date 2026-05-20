/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

// GitHub App authentication helper.
//
// Used by the AutoResearchProject reconciler (and any future caller
// that needs to push to per-agent gitops / wiki repos) instead of
// the old per-agent PAT pattern. Lets us delete the wikiGitToken
// form field on the karpathy template and adopt one shared bot
// identity (the `agent-pipelines-as-code` App, App ID 3559968) for
// the whole platform.
//
// The cluster-side state lives in a single K8s Secret named
// `agent-office-github-app` in the `agent-office` namespace:
//
//   appId           — text, e.g. "3559968"
//   installationId  — text, e.g. "128488425"
//   privateKey      — PEM-encoded RSA private key (one of the keys
//                     attached to the App; rotation is non-breaking
//                     because GitHub Apps support up to 25 keys at
//                     once)
//
// On each call we sign a JWT with the private key, exchange it for
// a 1-hour installation token via GitHub's REST API, and return it
// wrapped as go-git BasicAuth. Installation tokens are cheap to
// mint (~one HTTP round trip) so v1 does no caching; if the wiki-
// push hot path ever gets noisy we can layer in a 50-minute cache.
//
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get,resourceNames=agent-office-github-app

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

const (
	// GitHubAppSecretName / GitHubAppSecretNamespace identify the
	// cluster-shared App credentials Secret. The Namespace matches
	// where AutoResearchProject + KnowledgeBase CRs live — keeps
	// RBAC narrow.
	GitHubAppSecretName      = "agent-office-github-app"
	GitHubAppSecretNamespace = "agent-office"

	// jwtTTL is bounded by GitHub: max 10 minutes (we use 9 to
	// allow clock skew). The minted JWT is only used to mint a
	// longer-lived installation token, then discarded.
	jwtTTL = 9 * time.Minute
)

// githubAppCreds is the loaded-and-parsed shape of the K8s Secret.
type githubAppCreds struct {
	AppID          string
	InstallationID string
	PrivateKey     *rsa.PrivateKey
}

// LoadGitHubAppCreds reads + parses the cluster-shared App Secret.
// Returns a typed error if any field is missing so the caller can
// fall back to a per-agent token (legacy path) cleanly.
func LoadGitHubAppCreds(ctx context.Context, c client.Client) (*githubAppCreds, error) {
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: GitHubAppSecretNamespace,
		Name:      GitHubAppSecretName,
	}, &sec); err != nil {
		return nil, fmt.Errorf("get %s/%s: %w",
			GitHubAppSecretNamespace, GitHubAppSecretName, err)
	}
	appID := strings.TrimSpace(string(sec.Data["appId"]))
	installID := strings.TrimSpace(string(sec.Data["installationId"]))
	pemBytes := sec.Data["privateKey"]
	if appID == "" {
		return nil, fmt.Errorf("github-app Secret missing appId key")
	}
	if installID == "" {
		return nil, fmt.Errorf("github-app Secret missing installationId key")
	}
	if len(pemBytes) == 0 {
		return nil, fmt.Errorf("github-app Secret missing privateKey key")
	}

	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("privateKey is not valid PEM")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS1 RSA key: %w", err)
		}
		key = k
	case "PRIVATE KEY":
		// PKCS8-wrapped (GitHub Apps also generate this format
		// for newer keys).
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS8 key: %w", err)
		}
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key is not RSA (got %T)", k)
		}
		key = rsaKey
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q", block.Type)
	}

	return &githubAppCreds{
		AppID:          appID,
		InstallationID: installID,
		PrivateKey:     key,
	}, nil
}

// signAppJWT mints a short-lived (≤10 min) JWT identifying the App
// to GitHub. Used only to exchange for an installation token.
//
// Header: {"alg":"RS256","typ":"JWT"}
// Payload: {"iat":<now-60>, "exp":<now+jwtTTL>, "iss":<appID>}
// Signature: RS256 over `header.payload`.
//
// We back-date iat by 60s because GitHub's clock has been observed
// to reject JWTs whose iat is in the future after any client→server
// skew. This is the documented mitigation.
func signAppJWT(creds *githubAppCreds, now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	payload := map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(jwtTTL).Unix(),
		"iss": creds.AppID,
	}
	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(payload)

	enc := func(b []byte) string {
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(headerJSON) + "." + enc(payloadJSON)

	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, creds.PrivateKey, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	return signingInput + "." + enc(sig), nil
}

// installationTokenResponse mirrors GitHub's reply shape (subset).
type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// MintInstallationToken exchanges a freshly-signed App JWT for an
// installation-scoped access token. The token is good for ~1 hour
// and carries the App's permissions on the installation's repos.
func MintInstallationToken(ctx context.Context, creds *githubAppCreds) (string, error) {
	jwt, err := signAppJWT(creds, time.Now())
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", creds.InstallationID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github API: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("mint installation token: HTTP %d %s",
			resp.StatusCode, truncateMsg(string(body), 240))
	}
	var out installationTokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode installation token response: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("github returned empty token")
	}
	return out.Token, nil
}

// GitHubAppBasicAuth is the convenience wrapper most callers want:
// reads the K8s Secret, mints a fresh installation token, and
// returns it wrapped as go-git BasicAuth. The token lives in
// memory only — never written to disk, never logged.
//
// GitHub accepts installation tokens via either:
//   Authorization: Bearer <token>    (REST API)
//   https://x-access-token:<token>@github.com/...   (git over HTTPS)
//
// go-git's BasicAuth maps to the second form when used over HTTPS,
// which is what we use everywhere for clone/push.
func GitHubAppBasicAuth(ctx context.Context, c client.Client) (*githttp.BasicAuth, error) {
	creds, err := LoadGitHubAppCreds(ctx, c)
	if err != nil {
		return nil, err
	}
	token, err := MintInstallationToken(ctx, creds)
	if err != nil {
		return nil, err
	}
	return &githttp.BasicAuth{
		Username: "x-access-token",
		Password: token,
	}, nil
}
