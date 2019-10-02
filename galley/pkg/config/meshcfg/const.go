// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package meshcfg

import (
	"istio.io/istio/galley/pkg/config/resource"
	"istio.io/istio/galley/pkg/config/schema/collection"
)

// IstioMeshconfig is the name of collection istio/meshconfig
// It is captured here explicitly, as some of the core pieces of code need to reference this.
var IstioMeshconfig = collection.NewName("istio/mesh/v1alpha1/MeshConfig")

// ResourceName for the Istio Mesh Config resource
var ResourceName = resource.NewName("istio-system", "meshconfig")
