/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"time"
)

// defaultInsecureTransport returns an http.Transport that skips
// TLS verification. Used only for the in-cluster DSP API URL
// (ds-pipeline-<name>.<ns>.svc.cluster.local:8443) — these
// connections never leave the cluster's pod network, so the
// usual untrusted-cert risk doesn't apply. Real fix down the
// line: load the cluster's internal CA bundle from
// /var/run/secrets/kubernetes.io/serviceaccount/ca.crt and use
// it as the RootCAs pool. v0.0.30 ships insecure as the
// pragmatic minimum.
func defaultInsecureTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
}

// insecureHTTPClient returns a short-timeout http.Client built on
// defaultInsecureTransport. Used by adapter-URI verification —
// the in-cluster Quay route presents a self-signed cert, same
// trust story as the DSP API call above.
//
// 10s timeout is intentional: we want the HEAD to fail FAST and
// surface "Unknown" so the round-drain loop isn't blocked when
// the registry is briefly unreachable.
func insecureHTTPClient() *http.Client {
	return &http.Client{
		Transport: defaultInsecureTransport(),
		Timeout:   10 * time.Second,
	}
}

// newHTTPRequestWithContext is a thin wrapper around
// http.NewRequestWithContext that drops the error return for the
// callers that can't usefully handle a malformed-URL error
// (verifyAdapterArtifact already validates the URI shape via
// splitOCIURI, so by the time we get here the URL is well-formed).
func newHTTPRequestWithContext(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, url, body)
}
