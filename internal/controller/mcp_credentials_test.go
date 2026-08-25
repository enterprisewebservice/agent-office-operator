/*
Copyright 2026.

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

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

func TestResolveMCPHeaderCredentials(t *testing.T) {
	data := map[string][]byte{
		"GITEA_TOKEN": []byte("tok-abc.123"),
		"OTHER":       []byte("zzz"),
	}

	t.Run("single resolvable ref becomes literal", func(t *testing.T) {
		out, n := resolveMCPHeaderCredentials(
			map[string]string{"Authorization": "Bearer ${GITEA_TOKEN}"}, data)
		if n != 1 {
			t.Fatalf("substitutions = %d, want 1", n)
		}
		if got := out["Authorization"]; got != "Bearer tok-abc.123" {
			t.Fatalf("Authorization = %q", got)
		}
	})

	t.Run("unresolvable ref stays literal for openclaw env expansion", func(t *testing.T) {
		out, n := resolveMCPHeaderCredentials(
			map[string]string{"Authorization": "Bearer ${NOT_IN_SECRET}"}, data)
		if n != 0 {
			t.Fatalf("substitutions = %d, want 0", n)
		}
		if got := out["Authorization"]; got != "Bearer ${NOT_IN_SECRET}" {
			t.Fatalf("Authorization = %q", got)
		}
	})

	t.Run("mixed refs substitute only what resolves", func(t *testing.T) {
		out, n := resolveMCPHeaderCredentials(map[string]string{
			"Authorization": "Bearer ${GITEA_TOKEN}",
			"X-Extra":       "${MISSING}/${OTHER}",
		}, data)
		if n != 2 {
			t.Fatalf("substitutions = %d, want 2", n)
		}
		if got := out["X-Extra"]; got != "${MISSING}/zzz" {
			t.Fatalf("X-Extra = %q", got)
		}
	})

	t.Run("nil data returns headers unchanged", func(t *testing.T) {
		in := map[string]string{"Authorization": "Bearer ${GITEA_TOKEN}"}
		out, n := resolveMCPHeaderCredentials(in, nil)
		if n != 0 || out["Authorization"] != "Bearer ${GITEA_TOKEN}" {
			t.Fatalf("out=%v n=%d", out, n)
		}
	})
}

func TestMCPServerConsumesSecretViaConfig(t *testing.T) {
	data := map[string][]byte{"GITEA_TOKEN": []byte("tok")}

	cases := []struct {
		name string
		srv  agentofficev1alpha1.MCPServerSpec
		want bool
	}{
		{
			name: "headers reference a secret key",
			srv: agentofficev1alpha1.MCPServerSpec{
				EnvFromSecret: "user1-gitea-token",
				Headers:       map[string]string{"Authorization": "Bearer ${GITEA_TOKEN}"},
			},
			want: true,
		},
		{
			name: "headers reference only foreign vars",
			srv: agentofficev1alpha1.MCPServerSpec{
				EnvFromSecret: "user1-gitea-token",
				Headers:       map[string]string{"Authorization": "Bearer ${SOMETHING_ELSE}"},
			},
			want: false,
		},
		{
			name: "no headers means pure env injection",
			srv: agentofficev1alpha1.MCPServerSpec{
				EnvFromSecret: "user1-gitea-token",
			},
			want: false,
		},
		{
			name: "no envFromSecret",
			srv: agentofficev1alpha1.MCPServerSpec{
				Headers: map[string]string{"Authorization": "Bearer ${GITEA_TOKEN}"},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpServerConsumesSecretViaConfig(&tc.srv, data); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCollectMCPEnvFromSecretsSkipsConfigDelivered proves the gateway
// reconciler keeps a config-delivered Secret in envFrom (so the pod still
// starts with the credential in its env — no migration roll) but drops it
// from the ROLL hash (so its rotation is hot-reloaded, not rolled), while a
// Secret used as a plain env injector stays in both (env + roll fallback).
func TestCollectMCPEnvFromSecretsSkipsConfigDelivered(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentofficev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	ns := "seat-ns"
	gw := &agentofficev1alpha1.AgentGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "seat-gw", Namespace: ns},
	}
	configSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "seat-gitea-token", Namespace: ns},
		Data:       map[string][]byte{"GITEA_TOKEN": []byte("tok")},
	}
	envSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "plain-env-creds", Namespace: ns},
		Data:       map[string][]byte{"SOME_VAR": []byte("v")},
	}
	aw := &agentofficev1alpha1.AgentWorkstation{
		ObjectMeta: metav1.ObjectMeta{Name: "seat-agent", Namespace: ns},
		Spec: agentofficev1alpha1.AgentWorkstationSpec{
			Runtime: &agentofficev1alpha1.RuntimeSpec{
				Shared: &agentofficev1alpha1.SharedRuntime{GatewayRef: "seat-gw"},
			},
			Tools: &agentofficev1alpha1.ToolsSpec{
				MCPServers: []agentofficev1alpha1.MCPServerSpec{
					{
						Name:          "governed",
						URL:           "http://mcp/mcp",
						Headers:       map[string]string{"Authorization": "Bearer ${GITEA_TOKEN}"},
						EnvFromSecret: "seat-gitea-token",
					},
					{
						Name:          "legacy",
						URL:           "http://mcp2/mcp",
						EnvFromSecret: "plain-env-creds",
					},
				},
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(gw, configSecret, envSecret, aw).Build()
	r := &AgentGatewayReconciler{Client: cl, Scheme: scheme}

	envSecrets, hashSecrets, err := r.collectMCPEnvFromSecrets(context.Background(), gw)
	if err != nil {
		t.Fatalf("collectMCPEnvFromSecrets: %v", err)
	}
	// envFrom carries BOTH (pod always starts with the credential in env).
	if len(envSecrets) != 2 || envSecrets[0] != "plain-env-creds" || envSecrets[1] != "seat-gitea-token" {
		t.Fatalf("envSecrets = %v, want [plain-env-creds seat-gitea-token]", envSecrets)
	}
	// hash carries ONLY the plain-env secret — the config-delivered one
	// must not roll the pod on rotation.
	if len(hashSecrets) != 1 || hashSecrets[0] != "plain-env-creds" {
		t.Fatalf("hashSecrets = %v, want [plain-env-creds] (config-delivered secret must not roll)", hashSecrets)
	}
}

// TestCollectMCPEnvFromSecretsMissingSecretFallsBackToEnv: a referenced
// Secret that cannot be read yet must stay in the ROLL hash — its later
// creation folds into the pod template and rolls the pod, which is how a
// created-after-the-CR credential still reaches the runtime env.
func TestCollectMCPEnvFromSecretsMissingSecretFallsBackToEnv(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentofficev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ns := "seat-ns"
	gw := &agentofficev1alpha1.AgentGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "seat-gw", Namespace: ns},
	}
	aw := &agentofficev1alpha1.AgentWorkstation{
		ObjectMeta: metav1.ObjectMeta{Name: "seat-agent", Namespace: ns},
		Spec: agentofficev1alpha1.AgentWorkstationSpec{
			Runtime: &agentofficev1alpha1.RuntimeSpec{
				Shared: &agentofficev1alpha1.SharedRuntime{GatewayRef: "seat-gw"},
			},
			Tools: &agentofficev1alpha1.ToolsSpec{
				MCPServers: []agentofficev1alpha1.MCPServerSpec{{
					Name:          "governed",
					URL:           "http://mcp/mcp",
					Headers:       map[string]string{"Authorization": "Bearer ${GITEA_TOKEN}"},
					EnvFromSecret: "not-created-yet",
				}},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw, aw).Build()
	r := &AgentGatewayReconciler{Client: cl, Scheme: scheme}

	envSecrets, hashSecrets, err := r.collectMCPEnvFromSecrets(context.Background(), gw)
	if err != nil {
		t.Fatalf("collectMCPEnvFromSecrets: %v", err)
	}
	// Unreadable → in envFrom AND in the roll hash (its creation must roll).
	if len(envSecrets) != 1 || envSecrets[0] != "not-created-yet" {
		t.Fatalf("envSecrets = %v, want [not-created-yet]", envSecrets)
	}
	if len(hashSecrets) != 1 || hashSecrets[0] != "not-created-yet" {
		t.Fatalf("hashSecrets = %v, want [not-created-yet]", hashSecrets)
	}
}
