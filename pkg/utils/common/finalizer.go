/*
Copyright 2018 The Kubernetes Authors.
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

package common

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/networking/v1beta1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	client "k8s.io/client-go/kubernetes/typed/networking/v1beta1"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/util/slice"
)

const (
	// FinalizerKey is the string representing the Ingress finalizer.
	FinalizerKey = "networking.gke.io/ingress-finalizer"
	// FinalizerKeyV2 is the string representing the Ingress finalizer version.
	// Ingress with V2 finalizer uses V2 frontend naming scheme.
	FinalizerKeyV2 = "networking.gke.io/ingress-finalizer-V2"
	// TODO remove the 2 definitions once they are added in legacy-cloud-providers/gce
	// LegacyILBFinalizer key is used to identify ILB services whose resources are managed by service controller.
	LegacyILBFinalizer = "gke.networking.io/l4-ilb-v1"
	// ILBFinalizerV2 is the finalizer used by newer controllers that implement Internal LoadBalancer services.
	ILBFinalizerV2 = "gke.networking.io/l4-ilb-v2"
	// NegFinalizerKey is the finalizer used by neg controller to ensure NEG CRs are deleted after corresponding negs are deleted
	NegFinalizerKey = "networking.gke.io/neg-finalizer"
)

// IsDeletionCandidate is true if the passed in meta contains an ingress finalizer.
func IsDeletionCandidate(m meta_v1.ObjectMeta) bool {
	return IsDeletionCandidateForGivenFinalizer(m, FinalizerKey) || IsDeletionCandidateForGivenFinalizer(m, FinalizerKeyV2)
}

// IsDeletionCandidateForGivenFinalizer is true if the passed in meta contains the specified finalizer.
func IsDeletionCandidateForGivenFinalizer(m meta_v1.ObjectMeta, key string) bool {
	return m.DeletionTimestamp != nil && HasGivenFinalizer(m, key)
}

// HasFinalizer is true if the passed in meta has an ingress finalizer.
func HasFinalizer(m meta_v1.ObjectMeta) bool {
	return HasGivenFinalizer(m, FinalizerKey) || HasGivenFinalizer(m, FinalizerKeyV2)
}

// HasGivenFinalizer is true if the passed in meta has the specified finalizer.
func HasGivenFinalizer(m meta_v1.ObjectMeta, key string) bool {
	return slice.ContainsString(m.Finalizers, key, nil)
}

// EnsureFinalizer ensures that the specified finalizer exists on given Ingress.
func EnsureFinalizer(ing *v1beta1.Ingress, ingClient client.IngressInterface, finalizerKey string) (*v1beta1.Ingress, error) {
	updated := ing.DeepCopy()
	if needToAddFinalizer(ing.ObjectMeta, finalizerKey) {
		updated.ObjectMeta.Finalizers = append(updated.ObjectMeta.Finalizers, finalizerKey)
		if _, err := PatchIngressObjectMetadata(ingClient, ing, updated.ObjectMeta); err != nil {
			return nil, fmt.Errorf("error patching Ingress %s/%s: %v", ing.Namespace, ing.Name, err)
		}
		klog.V(2).Infof("Added finalizer %q for Ingress %s/%s", finalizerKey, ing.Namespace, ing.Name)
	}
	return updated, nil
}

// needToAddFinalizer is true if the passed in meta does not contain the specified finalizer.
func needToAddFinalizer(m meta_v1.ObjectMeta, key string) bool {
	return m.DeletionTimestamp == nil && !HasGivenFinalizer(m, key)
}

// EnsureDeleteFinalizer ensures that the specified finalizer is deleted from given Ingress.
func EnsureDeleteFinalizer(ing *v1beta1.Ingress, ingClient client.IngressInterface, finalizerKey string) error {
	if HasGivenFinalizer(ing.ObjectMeta, finalizerKey) {
		updatedObjectMeta := ing.ObjectMeta.DeepCopy()
		updatedObjectMeta.Finalizers = slice.RemoveString(updatedObjectMeta.Finalizers, finalizerKey, nil)
		if _, err := PatchIngressObjectMetadata(ingClient, ing, *updatedObjectMeta); err != nil {
			return fmt.Errorf("error patching Ingress %s/%s: %v", ing.Namespace, ing.Name, err)
		}
		klog.V(2).Infof("Removed finalizer %q for Ingress %s/%s", finalizerKey, ing.Namespace, ing.Name)
	}
	return nil
}

// EnsureServiceFinalizer patches the service to add finalizer.
func EnsureServiceFinalizer(service *corev1.Service, key string, kubeClient kubernetes.Interface) error {
	if HasGivenFinalizer(service.ObjectMeta, key) {
		return nil
	}

	// Make a copy so we don't mutate the shared informer cache.
	updated := service.DeepCopy()
	updated.ObjectMeta.Finalizers = append(updated.ObjectMeta.Finalizers, key)

	klog.V(2).Infof("Adding finalizer %s to service %s/%s", key, updated.Namespace, updated.Name)
	return patchServiceFinalizer(kubeClient.CoreV1().Services(updated.Namespace), service, updated)
}

// removeFinalizer patches the service to remove finalizer.
func EnsureDeleteServiceFinalizer(service *corev1.Service, key string, kubeClient kubernetes.Interface) error {
	if !HasGivenFinalizer(service.ObjectMeta, key) {
		return nil
	}

	// Make a copy so we don't mutate the shared informer cache.
	updated := service.DeepCopy()
	updated.ObjectMeta.Finalizers = slice.RemoveString(updated.ObjectMeta.Finalizers, key, nil)

	klog.V(2).Infof("Removing finalizer from service %s/%s", updated.Namespace, updated.Name)
	return patchServiceFinalizer(kubeClient.CoreV1().Services(updated.Namespace), service, updated)
}

func patchServiceFinalizer(sc coreclient.ServiceInterface, oldSvc, newSvc *corev1.Service) error {
	svcKey := fmt.Sprintf("%s/%s", oldSvc.Namespace, oldSvc.Name)
	oldData, err := json.Marshal(oldSvc)
	if err != nil {
		return fmt.Errorf("failed to Marshal oldData for service %s: %v", svcKey, err)
	}

	newData, err := json.Marshal(newSvc)
	if err != nil {
		return fmt.Errorf("failed to Marshal newData for service %s: %v", svcKey, err)
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, corev1.Service{})
	if err != nil {
		return fmt.Errorf("failed to create TwoWayMergePatch for service %s: %v", svcKey, err)
	}

	klog.V(3).Infof("Patch bytes for service %s: %s", svcKey, patchBytes)
	_, err = sc.Patch(context.TODO(), oldSvc.Name, types.StrategicMergePatchType, patchBytes, meta_v1.PatchOptions{}, "status")
	return err
}
