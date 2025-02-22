/*
Copyright 2022 The KCP Authors.

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

package apiexportendpointslice

import (
	"context"
	"fmt"
	"net/url"
	"path"

	"github.com/kcp-dev/logicalcluster/v3"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	virtualworkspacesoptions "github.com/kcp-dev/kcp/cmd/virtual-workspaces/options"
	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	corev1alpha1 "github.com/kcp-dev/kcp/pkg/apis/core/v1alpha1"
	conditionsv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/third_party/conditions/apis/conditions/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/apis/third_party/conditions/util/conditions"
	"github.com/kcp-dev/kcp/pkg/logging"
	apiexportbuilder "github.com/kcp-dev/kcp/pkg/virtual/apiexport/builder"
)

type endpointsReconciler struct {
	listShards   func() ([]*corev1alpha1.Shard, error)
	getAPIExport func(path logicalcluster.Path, name string) (*apisv1alpha1.APIExport, error)
}

func (c *controller) reconcile(ctx context.Context, apiExportEndpointSlice *apisv1alpha1.APIExportEndpointSlice) error {
	r := &endpointsReconciler{
		listShards:   c.listShards,
		getAPIExport: c.getAPIExport,
	}

	return r.reconcile(ctx, apiExportEndpointSlice)
}

func (r *endpointsReconciler) reconcile(ctx context.Context, apiExportEndpointSlice *apisv1alpha1.APIExportEndpointSlice) error {
	// TODO (fgiloux): When the information is available in the cache server
	// check if at least one APIBinding is bound in the shard to the APIExport referenced by the APIExportEndpointSLice.
	// If so, add the respective endpoint to the status.
	// For now the unfiltered list is added.

	// Get APIExport
	apiExportPath := logicalcluster.NewPath(apiExportEndpointSlice.Spec.APIExport.Path)
	if apiExportPath.Empty() {
		apiExportPath = logicalcluster.From(apiExportEndpointSlice).Path()
	}
	apiExport, err := r.getAPIExport(apiExportPath, apiExportEndpointSlice.Spec.APIExport.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			// Don't keep the endpoints if the APIExport has been deleted
			apiExportEndpointSlice.Status.APIExportEndpoints = nil
			conditions.MarkFalse(
				apiExportEndpointSlice,
				apisv1alpha1.APIExportValid,
				apisv1alpha1.APIExportNotFoundReason,
				conditionsv1alpha1.ConditionSeverityError,
				"APIExport %s|%s not found",
				apiExportPath,
				apiExportEndpointSlice.Spec.APIExport.Name,
			)
			return nil
		} else {
			conditions.MarkFalse(
				apiExportEndpointSlice,
				apisv1alpha1.APIExportValid,
				apisv1alpha1.InternalErrorReason,
				conditionsv1alpha1.ConditionSeverityError,
				"Error getting APIExport %s|%s",
				apiExportPath,
				apiExportEndpointSlice.Spec.APIExport.Name,
			)
			return err
		}
	}
	conditions.MarkTrue(apiExportEndpointSlice, apisv1alpha1.APIExportValid)

	if err = r.updateEndpoints(ctx, apiExportEndpointSlice, apiExport); err != nil {
		conditions.MarkFalse(
			apiExportEndpointSlice,
			apisv1alpha1.APIExportEndpointSliceURLsReady,
			apisv1alpha1.ErrorGeneratingURLsReason,
			conditionsv1alpha1.ConditionSeverityError,
			err.Error(),
		)
		return err
	}
	conditions.MarkTrue(apiExportEndpointSlice, apisv1alpha1.APIExportEndpointSliceURLsReady)

	return nil
}

func (r *endpointsReconciler) updateEndpoints(ctx context.Context,
	apiExportEndpointSlice *apisv1alpha1.APIExportEndpointSlice,
	apiExport *apisv1alpha1.APIExport) error {
	logger := klog.FromContext(ctx)
	shards, err := r.listShards()
	if err != nil {
		return fmt.Errorf("error listing Shards: %w", err)
	}

	desiredURLs := sets.NewString()
	for _, shard := range shards {
		logger = logging.WithObject(logger, shard)
		if shard.Spec.VirtualWorkspaceURL == "" {
			continue
		}

		u, err := url.Parse(shard.Spec.VirtualWorkspaceURL)
		if err != nil {
			// Should never happen
			logger.Error(
				err, "error parsing shard.spec.virtualWorkspaceURL",
				"VirtualWorkspaceURL", shard.Spec.VirtualWorkspaceURL,
			)

			continue
		}

		u.Path = path.Join(
			u.Path,
			virtualworkspacesoptions.DefaultRootPathPrefix,
			apiexportbuilder.VirtualWorkspaceName,
			logicalcluster.From(apiExport).String(),
			apiExport.Name,
		)

		desiredURLs.Insert(u.String())
	}

	apiExportEndpointSlice.Status.APIExportEndpoints = nil
	for _, u := range desiredURLs.List() {
		apiExportEndpointSlice.Status.APIExportEndpoints = append(apiExportEndpointSlice.Status.APIExportEndpoints, apisv1alpha1.APIExportEndpoint{
			URL: u,
		})
	}

	return nil
}
