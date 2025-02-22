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

package plugins

import (
	"github.com/clusternet/clusternet/pkg/scheduler/framework/plugins/defaultbinder"
	"github.com/clusternet/clusternet/pkg/scheduler/framework/plugins/names"
	"github.com/clusternet/clusternet/pkg/scheduler/framework/plugins/tainttoleration"
	"github.com/clusternet/clusternet/pkg/scheduler/framework/runtime"
)

// NewInTreeRegistry builds the registry with all the in-tree plugins.
func NewInTreeRegistry() runtime.Registry {
	return runtime.Registry{
		names.DefaultBinder:   defaultbinder.New,
		names.TaintToleration: tainttoleration.New,
	}
}
