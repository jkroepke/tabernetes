package main

import (
	"bytes"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	_ "k8s.io/kubernetes/pkg/apis/apps/install"
)

// The seed is from the build: its objects must look as new as the cluster.
func TestFreshTimes(t *testing.T) {
	old := metav1.NewTime(time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC))
	now := time.Date(2026, 11, 15, 18, 30, 5, 0, time.UTC)
	ds := &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-proxy", Namespace: "kube-system", UID: "u1",
			CreationTimestamp: old, Labels: map[string]string{"k8s-app": "kube-proxy"},
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply, Time: &old},
				{Manager: "x", Operation: metav1.ManagedFieldsOperationUpdate, Time: &old},
			},
		},
		Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}},
	}
	s := protobuf.NewSerializer(legacyscheme.Scheme, legacyscheme.Scheme)
	var buf bytes.Buffer
	if err := s.Encode(ds, &buf); err != nil {
		t.Fatal(err)
	}
	out := freshTimes(buf.Bytes(), now)
	obj, _, err := s.Decode(out, nil, &appsv1.DaemonSet{})
	if err != nil {
		t.Fatal(err)
	}
	got := obj.(*appsv1.DaemonSet)
	if !got.CreationTimestamp.Time.Equal(now) {
		t.Errorf("creationTimestamp = %v", got.CreationTimestamp)
	}
	for _, f := range got.ManagedFields {
		if !f.Time.Time.Equal(now) {
			t.Errorf("managedFields time = %v", f.Time)
		}
	}
	got.CreationTimestamp, got.ManagedFields[0].Time, got.ManagedFields[1].Time = old, &old, &old
	got.TypeMeta = ds.TypeMeta
	var again bytes.Buffer
	if err := s.Encode(got, &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Bytes(), buf.Bytes()) {
		t.Error("other fields changed")
	}

	// Custom resources are JSON.
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("example.com/v1")
	u.SetKind("Thing")
	u.SetName("a")
	u.SetCreationTimestamp(old)
	j, _ := u.MarshalJSON()
	out = freshTimes(j, now)
	u2 := &unstructured.Unstructured{}
	if err := u2.UnmarshalJSON(out); err != nil {
		t.Fatal(err)
	}
	if !u2.GetCreationTimestamp().Time.Equal(now) || u2.GetName() != "a" {
		t.Errorf("JSON: %s", out)
	}

	// Anything else stays as it is.
	for _, v := range [][]byte{nil, []byte("plain"), []byte("k8s\x00\xff\xff")} {
		if got := freshTimes(v, now); !bytes.Equal(got, v) {
			t.Errorf("freshTimes(%q) = %q", v, got)
		}
	}
}
