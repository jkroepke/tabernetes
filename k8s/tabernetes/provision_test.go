package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestNewVolume(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "data-db-0", UID: "1234"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Mi")}},
		},
	}
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard"}, Provisioner: provisionerName, ReclaimPolicy: ptr.To(corev1.PersistentVolumeReclaimRetain)}
	pv := newVolume(pvc, sc)
	if pv.Name != "pvc-1234" || pv.Spec.ClaimRef.Name != "data-db-0" || pv.Spec.ClaimRef.UID != "1234" {
		t.Errorf("name or claim: %s %+v", pv.Name, pv.Spec.ClaimRef)
	}
	if q := pv.Spec.Capacity[corev1.ResourceStorage]; q.String() != "10Mi" {
		t.Errorf("capacity %s", q.String())
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain || pv.Spec.StorageClassName != "standard" {
		t.Errorf("reclaim %s, class %s", pv.Spec.PersistentVolumeReclaimPolicy, pv.Spec.StorageClassName)
	}
	if pv.Annotations[annProvisionedBy] != provisionerName || pv.Spec.Local == nil {
		t.Errorf("annotations %v, source %+v", pv.Annotations, pv.Spec.PersistentVolumeSource)
	}
	req := pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0]
	if req.Key != corev1.LabelOSStable || req.Values[0] != "browser" {
		t.Errorf("node affinity %+v", req)
	}
}
