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

package migration

import (
	"context"
	"errors"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	csiv1 "github.com/ceph/ceph-csi-operator/api/v1"
)

const (
	testCRD = "cephconnections.csi.ceph.io"

	removedVersion = "v1alpha1"
)

func newCRD(storage string, stored ...string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: testCRD, Generation: 1},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "csi.ceph.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "cephconnections",
				Kind:     "CephConnection",
				ListKind: "CephConnectionList",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true, Storage: storage == "v1"},
				{Name: removedVersion, Served: false, Storage: storage == removedVersion},
			},
		},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{StoredVersions: stored},
	}
}

func newConn(ns, name string) *csiv1.CephConnection {
	return &csiv1.CephConnection{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       csiv1.CephConnectionSpec{Monitors: []string{"192.0.2.1:3300"}},
	}
}

func newMigrator(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (*StoredVersionMigrator, *int) {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(csiv1.AddToScheme(scheme))

	updates := 0
	countingUpdate := funcs.Update
	funcs.Update = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, isCRD := obj.(*apiextensionsv1.CustomResourceDefinition); !isCRD {
			updates++
		}
		if countingUpdate != nil {
			return countingUpdate(ctx, c, obj, opts...)
		}
		return c.Update(ctx, obj, opts...)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiextensionsv1.CustomResourceDefinition{}).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		Build()
	return &StoredVersionMigrator{
		Client:        c,
		Group:         "csi.ceph.io",
		Resources:     []string{"cephconnections"},
		RemoveVersion: removedVersion,
	}, &updates
}

func storedVersions(t *testing.T, m *StoredVersionMigrator) []string {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := m.Client.Get(context.Background(), client.ObjectKey{Name: testCRD}, crd); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	return crd.Status.StoredVersions
}

func TestMigrateCRDRewritesObjectsAndPrunesStoredVersions(t *testing.T) {
	m, updates := newMigrator(t, interceptor.Funcs{},
		newCRD("v1", "v1", removedVersion),
		newConn("ns-a", "one"), newConn("ns-b", "two"))

	if err := m.migrateCRD(context.Background(), testCRD); err != nil {
		t.Fatalf("migrateCRD: %v", err)
	}
	if *updates != 2 {
		t.Errorf("expected 2 object rewrites, got %d", *updates)
	}
	if got := storedVersions(t, m); !slices.Equal(got, []string{"v1"}) {
		t.Errorf("expected storedVersions [v1], got %v", got)
	}
}

func TestMigrateCRDNoopWhenVersionNotStored(t *testing.T) {
	m, updates := newMigrator(t, interceptor.Funcs{},
		newCRD("v1", "v1"), newConn("ns-a", "one"))

	if err := m.migrateCRD(context.Background(), testCRD); err != nil {
		t.Fatalf("migrateCRD: %v", err)
	}
	if *updates != 0 {
		t.Errorf("expected no rewrites, got %d", *updates)
	}
	if got := storedVersions(t, m); !slices.Equal(got, []string{"v1"}) {
		t.Errorf("expected storedVersions [v1], got %v", got)
	}
}

func TestMigrateCRDRefusesWhenRemoveVersionIsStorage(t *testing.T) {
	m, updates := newMigrator(t, interceptor.Funcs{},
		newCRD(removedVersion, "v1", removedVersion), newConn("ns-a", "one"))

	if err := m.migrateCRD(context.Background(), testCRD); err == nil {
		t.Fatal("expected an error when v1alpha1 is the storage version")
	}
	if *updates != 0 {
		t.Errorf("expected no rewrites, got %d", *updates)
	}
	if got := storedVersions(t, m); !slices.Equal(got, []string{"v1", removedVersion}) {
		t.Errorf("storedVersions must be untouched, got %v", got)
	}
}

func TestMigrateCRDKeepsStoredVersionsWhenRewriteFails(t *testing.T) {
	m, _ := newMigrator(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == "two" {
				return errors.New("injected failure")
			}
			return c.Update(ctx, obj, opts...)
		},
	}, newCRD("v1", "v1", removedVersion), newConn("ns-a", "one"), newConn("ns-b", "two"))

	if err := m.migrateCRD(context.Background(), testCRD); err == nil {
		t.Fatal("expected an error when an object rewrite fails")
	}
	if got := storedVersions(t, m); !slices.Equal(got, []string{"v1", removedVersion}) {
		t.Errorf("storedVersions must be untouched after a failed rewrite, got %v", got)
	}
}
