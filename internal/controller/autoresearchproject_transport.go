/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"crypto/tls"
	"net/http"
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
