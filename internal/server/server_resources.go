package server

import (
	"context"

	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
)

// resourceDeps wires the resource-level Application routes (resource tree, managed
// resources, server-side diff, events, live resources, actions, logs, terminal): the
// controller tree API for controller-owned state and the destination cluster
// configurations of the cluster registry (the cluster's own credentials; the handlers add
// impersonation when the settings require it). It is the only place that depends on the
// shape of applications.ResourceDeps, which is owned by the application resource routes.
func resourceDeps(tree *treeapi.Client, registry *clusters.Registry, opts clusters.Options) applications.ResourceDeps {
	return applications.ResourceDeps{
		Tree: tree,
		Clusters: applications.ClusterConfigsFunc(func(ctx context.Context, c *v1alpha1.Cluster) (*rest.Config, error) {
			return registry.RestConfig(ctx, c, opts)
		}),
	}
}
