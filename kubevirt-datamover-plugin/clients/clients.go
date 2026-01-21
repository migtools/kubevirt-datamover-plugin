// Copyright 2026 Red Hat Inc.
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

package clients

import (
	"k8s.io/client-go/discovery"
	appsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

var coreClient *corev1.CoreV1Client
var coreClientError error

var appsClient *appsv1.AppsV1Client
var appsClientError error

var discoveryClient *discovery.DiscoveryClient
var discoveryClientError error

var inClusterConfig *rest.Config

// SetInClusterConfig allows setting a custom in-cluster config (useful for testing)
func SetInClusterConfig(config *rest.Config) {
	inClusterConfig = config
}

// GetInClusterConfig returns the in-cluster config, creating it if necessary
func GetInClusterConfig() (*rest.Config, error) {
	if inClusterConfig != nil {
		return inClusterConfig, nil
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	inClusterConfig = config
	return inClusterConfig, nil
}

// CoreClient returns a kubernetes CoreV1Client (singleton)
func CoreClient() (*corev1.CoreV1Client, error) {
	if coreClient == nil && coreClientError == nil {
		coreClient, coreClientError = newCoreClient()
	}
	return coreClient, coreClientError
}

// CoreClientFromConfig creates a CoreV1Client from the given config
func CoreClientFromConfig(config *rest.Config) (*corev1.CoreV1Client, error) {
	client, err := corev1.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	coreClient = client
	return client, nil
}

func newCoreClient() (*corev1.CoreV1Client, error) {
	config, err := GetInClusterConfig()
	if err != nil {
		return nil, err
	}
	client, err := corev1.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// AppsClient returns a kubernetes AppsV1Client (singleton)
func AppsClient() (*appsv1.AppsV1Client, error) {
	if appsClient == nil && appsClientError == nil {
		appsClient, appsClientError = newAppsClient()
	}
	return appsClient, appsClientError
}

func newAppsClient() (*appsv1.AppsV1Client, error) {
	config, err := GetInClusterConfig()
	if err != nil {
		return nil, err
	}
	client, err := appsv1.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// DiscoveryClient returns a client-go DiscoveryClient (singleton)
func DiscoveryClient() (*discovery.DiscoveryClient, error) {
	if discoveryClient == nil && discoveryClientError == nil {
		discoveryClient, discoveryClientError = newDiscoveryClient()
	}
	return discoveryClient, discoveryClientError
}

func newDiscoveryClient() (*discovery.DiscoveryClient, error) {
	config, err := GetInClusterConfig()
	if err != nil {
		return nil, err
	}
	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// Add more client factories here as needed
// Example for KubeVirt client:
//
// var kubevirtClient *kubevirtclient.KubevirtV1Client
// var kubevirtClientError error
//
// func KubevirtClient() (*kubevirtclient.KubevirtV1Client, error) {
//     if kubevirtClient == nil && kubevirtClientError == nil {
//         kubevirtClient, kubevirtClientError = newKubevirtClient()
//     }
//     return kubevirtClient, kubevirtClientError
// }
//
// func newKubevirtClient() (*kubevirtclient.KubevirtV1Client, error) {
//     config, err := GetInClusterConfig()
//     if err != nil {
//         return nil, err
//     }
//     client, err := kubevirtclient.NewForConfig(config)
//     if err != nil {
//         return nil, err
//     }
//     return client, nil
// }

func init() {
	coreClient, coreClientError = nil, nil
	appsClient, appsClientError = nil, nil
	discoveryClient, discoveryClientError = nil, nil
}
