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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// AgentWorkstationReconciler reconciles a AgentWorkstation object
type AgentWorkstationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations/finalizers,verbs=update

// Reconcile is intentionally a no-op in slice 2.
//
// AgentWorkstation lifecycle is currently owned by agent-office-server
// (see agent-office/backend/k8s/agents.go), which:
//   - creates ConfigMap/Secret/PVC/Deployment/Service on UI request
//   - watches AgentWorkstation events to maintain an in-memory cache for
//     the office UI
//   - patches status.phase / status.gatewayEndpoint after Deployment Ready
//
// Slice 3 will migrate that ownership into this controller — once the
// operator owns the dependents, agent-office-server can drop its
// pseudo-controller goroutine and become a pure read/proxy client of
// the agent-office.ai API. The MemoryModule reconciler still works on
// AgentWorkstations (read-only, via Watches) to maintain the
// referencedBy index, so removing AgentWorkstation reconciliation from
// here doesn't lose the slice-2 visibility.
func (r *AgentWorkstationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentWorkstationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.AgentWorkstation{}).
		Named("agentworkstation").
		Complete(r)
}
