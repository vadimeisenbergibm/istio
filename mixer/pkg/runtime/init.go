// Copyright 2017 Istio Authors
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

package runtime

import (
	"context"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/glog"

	"istio.io/istio/mixer/pkg/adapter"
	cpb "istio.io/istio/mixer/pkg/config/proto"
	"istio.io/istio/mixer/pkg/config/store"
	"istio.io/istio/mixer/pkg/expr"
	"istio.io/istio/mixer/pkg/pool"
	"istio.io/istio/mixer/pkg/template"
)

// This file contains code to create new objects that are
// of package wide interest.

// New creates a new runtime Dispatcher
// Create a new controller and a dispatcher.
// Returns a ready to use dispatcher.
func New(eval expr.Evaluator, gp *pool.GoroutinePool, handlerPool *pool.GoroutinePool,
	identityAttribute string, defaultConfigNamespace string,
	s store.Store2, adapterInfo map[string]*adapter.Info,
	templateInfo map[string]template.Info) (Dispatcher, error) {
	// controller will set Resolver before the dispatcher is used.
	d := newDispatcher(eval, nil, gp, identityAttribute)
	err := startController(s, adapterInfo, templateInfo, eval, d,
		identityAttribute, defaultConfigNamespace, handlerPool)

	return d, err
}

// startWatch registers with store, initiates a watch, and returns the current config state.
func startWatch(s store.Store2, adapterInfo map[string]*adapter.Info,
	templateInfo map[string]template.Info) (map[store.Key]*store.Resource, <-chan store.Event, error) {
	ctx := context.Background()
	kindMap := kindMap(adapterInfo, templateInfo)
	if err := s.Init(ctx, kindMap); err != nil {
		return nil, nil, err
	}
	// create channel before listing.
	watchChan, err := s.Watch(ctx)
	if err != nil {
		return nil, nil, err
	}
	return s.List(), watchChan, nil
}

// kindMap generates a map from object kind to its proto message.
func kindMap(adapterInfo map[string]*adapter.Info,
	templateInfo map[string]template.Info) map[string]proto.Message {
	kindMap := make(map[string]proto.Message)
	// typed instances
	for kind, info := range templateInfo {
		kindMap[kind] = info.CtrCfg
		glog.Infof("template Kind: %s, %v", kind, info.CtrCfg)
	}
	// typed handlers
	for kind, info := range adapterInfo {
		kindMap[kind] = info.DefaultConfig
		glog.Infof("adapter Kind: %s, %v", kind, info.DefaultConfig)
	}
	kindMap[RulesKind] = &cpb.Rule{}
	glog.Infof("template Kind: %s", RulesKind)
	kindMap[AttributeManifestKind] = &cpb.AttributeManifest{}
	glog.Infof("template Kind: %s", AttributeManifestKind)

	return kindMap
}

// startController creates a controller from the given params.
func startController(s store.Store2, adapterInfo map[string]*adapter.Info,
	templateInfo map[string]template.Info, eval expr.Evaluator,
	dispatcher ResolverChangeListener,
	identityAttribute string, defaultConfigNamespace string, handlerPool *pool.GoroutinePool) error {

	data, watchChan, err := startWatch(s, adapterInfo, templateInfo)
	if err != nil {
		return err
	}

	c := &Controller{
		adapterInfo:            adapterInfo,
		templateInfo:           templateInfo,
		eval:                   eval,
		configState:            data,
		dispatcher:             dispatcher,
		resolver:               &resolver{}, // get an empty resolver
		identityAttribute:      identityAttribute,
		defaultConfigNamespace: defaultConfigNamespace,
		handlerGoRoutinePool:   handlerPool,
		table:                  make(map[string]*HandlerEntry),
		createHandlerFactory:   newHandlerFactory,
	}

	c.publishSnapShot()
	glog.Infof("Config controller has started with %d config elements", len(c.configState))
	go watchChanges(watchChan, c.applyEvents)
	return nil
}
