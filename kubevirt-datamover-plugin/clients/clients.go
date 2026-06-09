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
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1scheme "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	kvcore "kubevirt.io/api/core/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

var coreClient corev1.CoreV1Interface
var coreClientError error

var crClient crclient.Client
var crClientError error

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
func CoreClient() (corev1.CoreV1Interface, error) {
	if coreClient == nil && coreClientError == nil {
		coreClient, coreClientError = newCoreClient()
	}
	return coreClient, coreClientError
}

// SetCoreClient allows setting a mock core client for testing.
// Pass nil to reset the client.
func SetCoreClient(client corev1.CoreV1Interface) {
	coreClient = client
	coreClientError = nil
}

// CoreClientFromConfig creates a CoreV1Client from the given config
func CoreClientFromConfig(config *rest.Config) (corev1.CoreV1Interface, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	coreClient = clientset.CoreV1()
	return coreClient, nil
}

func newCoreClient() (corev1.CoreV1Interface, error) {
	config, err := GetInClusterConfig()
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return clientset.CoreV1(), nil
}

// CRClient returns a controller-runtime client (singleton)
func CRClient() (crclient.Client, error) {
	if crClient == nil && crClientError == nil {
		crClient, crClientError = newCRClient()
	}
	return crClient, crClientError
}

// SetCRClient allows setting a mock controller-runtime client for testing.
// Pass nil to reset the client.
func SetCRClient(client crclient.Client) {
	crClient = client
	crClientError = nil
}

func newCRClient() (crclient.Client, error) {
	config, err := GetInClusterConfig()
	if err != nil {
		return nil, err
	}

	// Create scheme with core types registered for volumehelper
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := kvcore.AddToScheme(scheme); err != nil {
		return nil, err
	}
        corev1scheme.AddToScheme(scheme)
        velerov1.AddToScheme(scheme)

	client, err := crclient.New(config, crclient.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	return client, nil
}

func init() {
	coreClient, coreClientError = nil, nil
	crClient, crClientError = nil, nil
}
