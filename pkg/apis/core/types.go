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

package core

import (
	"github.com/kcp-dev/logicalcluster/v3"
)

const (
	// LogicalClusterPathAnnotationKey is the annotation key for the logical cluster path
	// put on objects that are referenced by path by other objects.
	//
	// If this annotation exists, the system will maintain the annotation value.
	LogicalClusterPathAnnotationKey = "kcp.io/path"
)

// RootCluster is the root of ClusterWorkspace based logical clusters.
var RootCluster = logicalcluster.Name("root")
