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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ubiv1alpha1 "github.com/yfxue/pi-agent-operator/api/v1alpha1"
)

// setCondition upserts a condition on the PiWorker status. Reason and Message are always
// populated because the Kubernetes API rejects conditions with an empty reason.
func setCondition(piworker *ubiv1alpha1.PiWorker, condType string, status bool) {
	condStatus := metav1.ConditionFalse
	reason := "False"
	message := "Condition is false"
	if status {
		condStatus = metav1.ConditionTrue
		reason = "True"
		message = "Condition is true"
	}
	for i, c := range piworker.Status.Conditions {
		if c.Type == condType {
			if c.Status == condStatus {
				return
			}
			piworker.Status.Conditions[i].Status = condStatus
			piworker.Status.Conditions[i].Reason = reason
			piworker.Status.Conditions[i].Message = message
			piworker.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now())
			piworker.Status.Conditions[i].ObservedGeneration = piworker.Generation
			return
		}
	}
	piworker.Status.Conditions = append(piworker.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.NewTime(time.Now()),
		ObservedGeneration: piworker.Generation,
	})
}
