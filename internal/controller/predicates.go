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
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ignoreStatusOnlyUpdates drops update events whose only change is the
// object's status (plus the resourceVersion and managedFields every
// write bumps). Create, delete and generic events pass, and so does any
// change to spec (generation), labels, annotations, finalizers, owners
// or the deletion timestamp.
//
// v1.7.77: without it, the gateway reconciler's status.lastActivity
// patch re-ran the whole AgentWorkstation reconcile — exec'ing into the
// gateway and rewriting openclaw.json — whose side effects looked like
// new activity, and around it went every few seconds. Neither
// reconciler reads an AgentWorkstation's status to decide anything it
// renders, and the agent reconcile requeues itself every 5 minutes.
func ignoreStatusOnlyUpdates() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, n := e.ObjectOld, e.ObjectNew
			if o == nil || n == nil {
				return true
			}
			return o.GetGeneration() != n.GetGeneration() ||
				!equality.Semantic.DeepEqual(o.GetLabels(), n.GetLabels()) ||
				!equality.Semantic.DeepEqual(o.GetAnnotations(), n.GetAnnotations()) ||
				!equality.Semantic.DeepEqual(o.GetFinalizers(), n.GetFinalizers()) ||
				!equality.Semantic.DeepEqual(o.GetOwnerReferences(), n.GetOwnerReferences()) ||
				!o.GetDeletionTimestamp().Equal(n.GetDeletionTimestamp())
		},
	}
}
