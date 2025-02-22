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

package known

// annotations
const (
	// AutoUpdateAnnotation is the name of an annotation which prevents reconciliation if set to "false"
	AutoUpdateAnnotation = "clusternet.io/autoupdate"

	// FeedProtectionAnnotation passes detailed message on protecting current object as a feed
	FeedProtectionAnnotation = "apps.clusternet.io/feed-protection"
)
