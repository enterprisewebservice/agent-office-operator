/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"crypto/rand"
	"encoding/base64"
	"os"
)

// DefaultOpenClawImage is the OpenClaw runtime image used when the AW
// spec doesn't pin one. The cluster Quay only has `openclaw:latest`
// today; `openclaw-browser:0.1.0` referenced in earlier code never
// existed there.
const DefaultOpenClawImage = "quay-quay-quay-test.apps.salamander.aimlworkbench.com/deanpeterson/openclaw:latest"

// appsDomain returns the cluster's apps domain (e.g.
// "apps.salamander.aimlworkbench.com") used to build the Route
// hostname for openclaw.json's allowedOrigins. Sourced from the
// CLUSTER_APPS_DOMAIN env var on the operator pod (set by the CSV's
// downward-API or a literal in manager.yaml). Empty string means the
// operator falls back to localhost-only allowedOrigins.
func appsDomain() string { return os.Getenv("CLUSTER_APPS_DOMAIN") }

// agentLabels are stamped onto every owned resource. The set MUST
// match the existing label set on the cluster (see the 5 pre-slice-4
// agents) because Deployment.spec.selector is immutable — changing
// even one label here breaks adoption with "field is immutable".
//
// managed-by stays "agent-office" (not "agent-office-operator") for
// compatibility with the office UI's selectors and Backstage's
// kubernetes-id linking.
func agentLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "agent-office",
		"app.kubernetes.io/name":       name,
		"agentoffice.ai/agent":         name,
		"backstage.io/kubernetes-id":   name,
	}
}

// generateGatewayToken creates a random URL-safe token for OpenClaw
// gateway auth. Operator generates this once and persists it in the
// per-agent token Secret so re-reconcile is idempotent.
func generateGatewayToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// mergeLabels overlays src onto dst, returning the merged map.
func mergeLabels(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
