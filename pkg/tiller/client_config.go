/*
Copyright 2014 The Kubernetes Authors.

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

package tiller

import (
	"io"
	"sync"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// DeferredLoadingClientConfig is a ClientConfig interface that is backed by a client config loader.
// It is used in cases where the loading rules may change after you've instantiated them and you want to be sure that
// the most recent rules are used.  This is useful in cases where you bind flags to loading rule parameters before
// the parse happens and you want your calling code to be ignorant of how the values are being mutated to avoid
// passing extraneous information down a call stack
type DeferredLoadingClientConfig struct {
	loader         clientcmd.ClientConfigLoader
	overrides      *clientcmd.ConfigOverrides
	fallbackReader io.Reader
	user           string
	groups         []string

	clientConfig clientcmd.ClientConfig
	loadingLock  sync.Mutex

	// provided for testing
	icc InClusterConfig
}

// InClusterConfig abstracts details of whether the client is running in a cluster for testing.
type InClusterConfig interface {
	clientcmd.ClientConfig
	Possible() bool
}

// NewNonInteractiveDeferredLoadingClientConfig creates a ConfigClientClientConfig using the passed context name
func NewImpersonationClientConfig(loader clientcmd.ClientConfigLoader, overrides *clientcmd.ConfigOverrides, user string, groups []string) clientcmd.ClientConfig {
	return &DeferredLoadingClientConfig{loader: loader, overrides: overrides, user: user, groups: groups}
}

//// NewInteractiveDeferredLoadingClientConfig creates a ConfigClientClientConfig using the passed context name and the fallback auth reader
//func NewImpersonationClientConfig(loader clientcmd.ClientConfigLoader, overrides *clientcmd.ConfigOverrides, fallbackReader io.Reader, user string, groups []string) clientcmd.ClientConfig {
//	return &DeferredLoadingClientConfig{loader: loader, overrides: overrides, fallbackReader: fallbackReader, user: user, groups: groups}
//}

func (config *DeferredLoadingClientConfig) createClientConfig() (clientcmd.ClientConfig, error) {
	if config.clientConfig == nil {
		config.loadingLock.Lock()
		defer config.loadingLock.Unlock()

		if config.clientConfig == nil {
			mergedConfig, err := config.loader.Load()
			if err != nil {
				return nil, err
			}

			var mergedClientConfig clientcmd.ClientConfig
			if config.fallbackReader != nil {
				mergedClientConfig = clientcmd.NewInteractiveClientConfig(*mergedConfig, config.overrides.CurrentContext, config.overrides, config.fallbackReader, config.loader)
			} else {
				mergedClientConfig = clientcmd.NewNonInteractiveClientConfig(*mergedConfig, config.overrides.CurrentContext, config.overrides, config.loader)
			}

			config.clientConfig = mergedClientConfig
		}
	}

	return config.clientConfig, nil
}

func (config *DeferredLoadingClientConfig) RawConfig() (clientcmdapi.Config, error) {
	mergedConfig, err := config.createClientConfig()
	if err != nil {
		return clientcmdapi.Config{}, err
	}

	return mergedConfig.RawConfig()
}

// ClientConfig implements ClientConfig
func (config *DeferredLoadingClientConfig) ClientConfig() (*restclient.Config, error) {
	mergedClientConfig, err := config.createClientConfig()
	if err != nil {
		return nil, err
	}

	// load the configuration and return on non-empty errors and if the
	// content differs from the default config
	mergedConfig, err := mergedClientConfig.ClientConfig()
	switch {
	case err != nil:
		if !clientcmd.IsEmptyConfig(err) {
			// return on any error except empty config
			return nil, err
		}
	case mergedConfig != nil:
		// the configuration is valid, but if this is equal to the defaults we should try
		// in-cluster configuration
		if !config.loader.IsDefaultConfig(mergedConfig) {
			return mergedConfig, nil
		}
	}

	// check for in-cluster configuration and use it
	if config.icc.Possible() {
		glog.V(4).Infof("Using in-cluster configuration")
		return config.icc.ClientConfig()
	}
	mergedConfig.Impersonate.UserName = config.user
	mergedConfig.Impersonate.Groups = config.groups

	// return the result of the merged client config
	return mergedConfig, err
}

// Namespace implements KubeConfig
func (config *DeferredLoadingClientConfig) Namespace() (string, bool, error) {
	mergedKubeConfig, err := config.createClientConfig()
	if err != nil {
		return "", false, err
	}

	ns, overridden, err := mergedKubeConfig.Namespace()
	// if we get an error and it is not empty config, or if the merged config defined an explicit namespace, or
	// if in-cluster config is not possible, return immediately
	if (err != nil && !clientcmd.IsEmptyConfig(err)) || overridden || !config.icc.Possible() {
		// return on any error except empty config
		return ns, overridden, err
	}

	if len(ns) > 0 {
		// if we got a non-default namespace from the kubeconfig, use it
		if ns != v1.NamespaceDefault {
			return ns, false, nil
		}

		// if we got a default namespace, determine whether it was explicit or implicit
		if raw, err := mergedKubeConfig.RawConfig(); err == nil {
			if context := raw.Contexts[raw.CurrentContext]; context != nil && len(context.Namespace) > 0 {
				return ns, false, nil
			}
		}
	}

	glog.V(4).Infof("Using in-cluster namespace")

	// allow the namespace from the service account token directory to be used.
	return config.icc.Namespace()
}

// ConfigAccess implements ClientConfig
func (config *DeferredLoadingClientConfig) ConfigAccess() clientcmd.ConfigAccess {
	return config.loader
}