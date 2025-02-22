/*
Copyright 2021 The Clusternet Authors.

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

package defaultbinder

import (
	"context"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	appsapi "github.com/clusternet/clusternet/pkg/apis/apps/v1alpha1"
	framework "github.com/clusternet/clusternet/pkg/scheduler/framework/interfaces"
	"github.com/clusternet/clusternet/pkg/scheduler/framework/plugins/names"
	"github.com/clusternet/clusternet/pkg/utils"
)

// DefaultBinder binds subscriptions to clusters using a clusternet client.
type DefaultBinder struct {
	handle framework.Handle
}

var _ framework.BindPlugin = &DefaultBinder{}

// New creates a DefaultBinder.
func New(_ runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	return &DefaultBinder{handle: handle}, nil
}

// Name returns the name of the plugin.
func (pl *DefaultBinder) Name() string {
	return names.DefaultBinder
}

// Bind binds subscriptions to clusters using the clusternet client.
func (pl *DefaultBinder) Bind(ctx context.Context, sub *appsapi.Subscription, namespacedClusters []string) *framework.Status {
	klog.V(3).InfoS("Attempting to bind subscription to clusters",
		"subscription", klog.KObj(sub), "clusters", namespacedClusters)

	// use an ordered list
	sort.SliceStable(namespacedClusters, func(i, j int) bool {
		return namespacedClusters[i] < namespacedClusters[j]
	})

	subCopy := sub.DeepCopy()
	subCopy.Status.BindingClusters = namespacedClusters
	subCopy.Status.SpecHash = utils.HashSubscriptionSpec(&subCopy.Spec)
	subCopy.Status.DesiredReleases = len(namespacedClusters)

	_, err := pl.handle.ClientSet().AppsV1alpha1().Subscriptions(sub.Namespace).UpdateStatus(ctx, subCopy, metav1.UpdateOptions{})
	if err != nil {
		return framework.AsStatus(err)
	}
	return nil
}
