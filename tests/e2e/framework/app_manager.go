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

package framework

import (
	"errors"
	"flag"
	"path/filepath"

	"istio.io/istio/pkg/log"
	"istio.io/istio/tests/util"
)

var useAutomaticInjection = flag.Bool("use-automatic-injection", false, "Use automatic injection instead of kube-inject for transparent proxy injection")

const (
	kubeInjectPrefix = "KubeInject"
)

// App gathers information for Hop app
type App struct {
	AppYamlTemplate string
	AppYamls        []string
	KubeInject      bool
	Template        interface{}
	deployedYamls   []string
}

// AppManager organize and deploy apps
type AppManager struct {
	Apps      []*App
	tmpDir    string
	namespace string
	istioctl  *Istioctl
	active    bool
}

// NewAppManager create a new AppManager
func NewAppManager(tmpDir, namespace string, istioctl *Istioctl) *AppManager {
	return &AppManager{
		namespace: namespace,
		tmpDir:    tmpDir,
		istioctl:  istioctl,
	}
}

// generateAppYaml deploy testing app from tmpl
func (am *AppManager) generateAppYaml(a *App) error {
	if a.AppYamlTemplate == "" {
		return nil
	}
	var err error
	var appYaml string

	appYaml, err = util.CreateTempfile(am.tmpDir, filepath.Base(a.AppYamlTemplate), yamlSuffix)
	if err != nil {
		log.Errorf("Failed to generate yaml %s: %v", a.AppYamlTemplate, err)
		return err
	}
	if err := util.Fill(appYaml, a.AppYamlTemplate, a.Template); err != nil {
		log.Errorf("Failed to generate yaml for template %s", a.AppYamlTemplate)
		return err
	}
	a.AppYamls = []string{appYaml}
	return nil
}

func (am *AppManager) deploy(a *App) error {
	if err := am.generateAppYaml(a); err != nil {
		return err
	}

	for _, appYaml := range a.AppYamls {

		finalYaml := appYaml
		if a.KubeInject && !*useAutomaticInjection {
			var err error
			finalYaml, err = util.CreateTempfile(am.tmpDir, kubeInjectPrefix, yamlSuffix)
			if err != nil {
				log.Errorf("CreateTempfile failed %v", err)
				return err
			}
			if err = am.istioctl.KubeInject(appYaml, finalYaml); err != nil {
				log.Errorf("KubeInject failed for yaml %s: %v", appYaml, err)
				return err
			}
		}
		if err := util.KubeApply(am.namespace, finalYaml); err != nil {
			log.Errorf("Kubectl apply %s failed", finalYaml)
			return err
		}

		a.deployedYamls = append(a.deployedYamls, finalYaml)
	}
	return nil
}

// Setup deploy apps
func (am *AppManager) Setup() error {
	am.active = true
	log.Info("Setting up apps")
	for _, a := range am.Apps {
		log.Infof("Setup %v", a)
		if err := am.deploy(a); err != nil {
			log.Errorf("error deploying %v: %v", a, err)
			return err
		}
	}
	return am.CheckDeployments()
}

// Teardown currently does nothing, only to satisfied cleanable{}
func (am *AppManager) Teardown() error {
	am.active = false
	return nil
}

// AddApp for automated deployment. Must be done before Setup call.
func (am *AppManager) AddApp(a *App) {
	am.Apps = append(am.Apps, a)
}

// DeployApp adds the app and deploys it to the system. Must be called after Setup call.
func (am *AppManager) DeployApp(a *App) error {
	if !am.active {
		return errors.New("function DeployApp must be called after Setup")
	}
	am.AddApp(a)
	return am.deploy(a)
}

// UndeployApp deletes the app from the system. Must be called after Setup call.
func (am *AppManager) UndeployApp(a *App) error {
	if !am.active {
		return errors.New("function UndeployApp must be called after Setup")
	}

	for _, deployedYaml := range a.deployedYamls {
		if err := util.KubeDelete(am.namespace, deployedYaml); err != nil {
			log.Errorf("Kubectl delete %s failed", deployedYaml)
			return err
		}
	}
	return nil
}

// CheckDeployments waits for a period for the deployments to be started.
func (am *AppManager) CheckDeployments() error {
	return util.CheckDeployments(am.namespace, maxDeploymentRolloutTime)
}
