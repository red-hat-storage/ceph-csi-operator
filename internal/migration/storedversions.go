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

// Package migration contains one-shot data migrations that the operator runs
// on startup to prepare the cluster for future API changes.
package migration

import (
	"context"
	"fmt"
	"slices"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TODO: remove this migration together with the v1alpha1 API version once
// every supported upgrade path goes through a release that ships it.

//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get,resourceNames=cephconnections.csi.ceph.io;clientprofilemappings.csi.ceph.io;clientprofiles.csi.ceph.io;drivers.csi.ceph.io;operatorconfigs.csi.ceph.io
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions/status,verbs=update,resourceNames=cephconnections.csi.ceph.io;clientprofilemappings.csi.ceph.io;clientprofiles.csi.ceph.io;drivers.csi.ceph.io;operatorconfigs.csi.ceph.io
//+kubebuilder:rbac:groups=csi.ceph.io,resources=cephconnections;clientprofilemappings;clientprofiles;drivers;operatorconfigs,verbs=list;update

const (
	listPageSize  = 500
	retryInterval = time.Minute
)

// StoredVersionMigrator rewrites every object of the given CRDs so that they
// are persisted in the current storage version, and then drops RemoveVersion
// from status.storedVersions of each CRD.
//
// Neither the API server nor OLM ever prune status.storedVersions, and OLM
// refuses to apply a CRD that drops a version still listed there. Running this
// migrator in the release before a version is removed lets the removal be
// upgraded to without manual steps.
type StoredVersionMigrator struct {
	// Client must be an uncached client, so that objects in every namespace
	// are visible and the latest resourceVersion is always used.
	Client client.Client
	// Group is the API group of the CRDs, e.g. "csi.ceph.io".
	Group string
	// Resources are the plural resource names of the CRDs to migrate.
	Resources []string
	// RemoveVersion is the version to drop from status.storedVersions.
	RemoveVersion string
}

// NeedLeaderElection makes the migrator run only on the elected leader.
func (m *StoredVersionMigrator) NeedLeaderElection() bool {
	return true
}

// Start implements manager.Runnable. It retries until every CRD has been
// migrated or the context is cancelled. A failure never stops the manager;
// it only means a later upgrade that removes RemoveVersion stays blocked.
func (m *StoredVersionMigrator) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("stored-version-migrator")
	ctx = log.IntoContext(ctx, logger)

	_ = wait.PollUntilContextCancel(ctx, retryInterval, true, func(ctx context.Context) (bool, error) {
		pending := false
		for _, resource := range m.Resources {
			if err := m.migrateCRD(ctx, resource+"."+m.Group); err != nil {
				logger.Error(err, "Stored version migration failed, will retry", "crd", resource+"."+m.Group)
				pending = true
			}
		}
		if !pending {
			logger.Info("Stored version migration complete", "removedVersion", m.RemoveVersion)
		}
		return !pending, nil
	})
	return nil
}

func (m *StoredVersionMigrator) migrateCRD(ctx context.Context, crdName string) error {
	logger := log.FromContext(ctx).WithValues("crd", crdName)

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: crdName}, crd); err != nil {
		return fmt.Errorf("failed to get CRD: %w", err)
	}
	if !slices.Contains(crd.Status.StoredVersions, m.RemoveVersion) {
		logger.V(1).Info("Version not in storedVersions, nothing to do", "version", m.RemoveVersion)
		return nil
	}

	storageVersion := getStorageVersion(crd)
	if storageVersion == "" || storageVersion == m.RemoveVersion {
		return fmt.Errorf("storage version is %q, refusing to migrate off %q", storageVersion, m.RemoveVersion)
	}
	generation := crd.Generation

	logger.Info("Rewriting objects in the storage version", "storageVersion", storageVersion,
		"storedVersions", crd.Status.StoredVersions)
	count, err := m.rewriteObjects(ctx, schema.GroupVersionKind{
		Group:   crd.Spec.Group,
		Version: storageVersion,
		Kind:    crd.Spec.Names.Kind,
	}, crd.Spec.Names.ListKind)
	if err != nil {
		return err
	}
	logger.Info("Rewrote objects", "count", count)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := m.Client.Get(ctx, client.ObjectKey{Name: crdName}, crd); err != nil {
			return err
		}
		// Objects were rewritten in storageVersion. If the CRD changed in the
		// meantime the rewrite may not be valid anymore, start over.
		if crd.Generation != generation || getStorageVersion(crd) != storageVersion {
			return fmt.Errorf("CRD changed while migrating (generation %d -> %d), retrying", generation, crd.Generation)
		}
		crd.Status.StoredVersions = slices.DeleteFunc(crd.Status.StoredVersions, func(v string) bool {
			return v == m.RemoveVersion
		})
		if err := m.Client.Status().Update(ctx, crd); err != nil {
			return err
		}
		logger.Info("Removed version from storedVersions", "version", m.RemoveVersion,
			"storedVersions", crd.Status.StoredVersions)
		return nil
	})
}

// rewriteObjects issues an unchanged update for every object of the list kind,
// which makes the API server re-encode it in the current storage version.
// Unstructured objects are used so the round trip does not drop any field.
func (m *StoredVersionMigrator) rewriteObjects(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	listKind string,
) (int, error) {
	listGVK := gvk.GroupVersion().WithKind(listKind)
	count := 0
	continueToken := ""
	for {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(listGVK)
		if err := m.Client.List(ctx, list, client.Limit(listPageSize), client.Continue(continueToken)); err != nil {
			return count, fmt.Errorf("failed to list %s: %w", listGVK.Kind, err)
		}
		for i := range list.Items {
			list.Items[i].SetGroupVersionKind(gvk)
			if err := m.rewriteObject(ctx, &list.Items[i]); err != nil {
				return count, err
			}
			count++
		}
		continueToken = list.GetContinue()
		if continueToken == "" {
			return count, nil
		}
	}
}

func (m *StoredVersionMigrator) rewriteObject(ctx context.Context, obj *unstructured.Unstructured) error {
	key := client.ObjectKeyFromObject(obj)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := m.Client.Update(ctx, obj)
		if apierrors.IsConflict(err) {
			// Someone else wrote the object, which also stores it in the
			// current storage version. Refresh and write again to be sure.
			if getErr := m.Client.Get(ctx, key, obj); getErr != nil {
				return getErr
			}
		}
		return err
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to rewrite %s %s: %w", obj.GetKind(), key, err)
	}
	return nil
}

func getStorageVersion(crd *apiextensionsv1.CustomResourceDefinition) string {
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			return v.Name
		}
	}
	return ""
}
