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
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

var coreClient *corev1.CoreV1Client
var coreClientError error

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

func init() {
	coreClient, coreClientError = nil, nil
}
