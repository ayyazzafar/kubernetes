/*
Copyright 2018 The Kubernetes Authors.

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

package admission

import (
	"io/ioutil"
	"net/http"
	"time"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission"
	webhookinit "k8s.io/apiserver/pkg/admission/plugin/webhook/initializer"
	"k8s.io/apiserver/pkg/server"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/util/webhook"
	cacheddiscovery "k8s.io/client-go/discovery/cached"
	externalinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	internalinformers "k8s.io/kubernetes/pkg/client/informers/informers_generated/internalversion"
	quotainstall "k8s.io/kubernetes/pkg/quota/v1/install"
)

type AdmissionConfig struct {
	CloudConfigFile      string
	LoopbackClientConfig *rest.Config
	ExternalInformers    externalinformers.SharedInformerFactory
	InternalInformers    internalinformers.SharedInformerFactory
}

func (c *AdmissionConfig) buildAuthnInfoResolver(proxyTransport *http.Transport) webhook.AuthenticationInfoResolverWrapper {
	webhookAuthResolverWrapper := func(delegate webhook.AuthenticationInfoResolver) webhook.AuthenticationInfoResolver {
		return &webhook.AuthenticationInfoResolverDelegator{
			ClientConfigForFunc: func(server string) (*rest.Config, error) {
				if server == "kubernetes.default.svc" {
					return c.LoopbackClientConfig, nil
				}
				return delegate.ClientConfigFor(server)
			},
			ClientConfigForServiceFunc: func(serviceName, serviceNamespace string) (*rest.Config, error) {
				if serviceName == "kubernetes" && serviceNamespace == v1.NamespaceDefault {
					return c.LoopbackClientConfig, nil
				}
				ret, err := delegate.ClientConfigForService(serviceName, serviceNamespace)
				if err != nil {
					return nil, err
				}
				if proxyTransport != nil && proxyTransport.DialContext != nil {
					ret.Dial = proxyTransport.DialContext
				}
				return ret, err
			},
		}
	}
	return webhookAuthResolverWrapper
}

func (c *AdmissionConfig) New(proxyTransport *http.Transport, serviceResolver webhook.ServiceResolver) ([]admission.PluginInitializer, server.PostStartHookFunc, error) {
	webhookAuthResolverWrapper := c.buildAuthnInfoResolver(proxyTransport)
	webhookPluginInitializer := webhookinit.NewPluginInitializer(webhookAuthResolverWrapper, serviceResolver)

	var cloudConfig []byte
	if c.CloudConfigFile != "" {
		var err error
		cloudConfig, err = ioutil.ReadFile(c.CloudConfigFile)
		if err != nil {
			glog.Fatalf("Error reading from cloud configuration file %s: %#v", c.CloudConfigFile, err)
		}
	}
	internalClient, err := internalclientset.NewForConfig(c.LoopbackClientConfig)
	if err != nil {
		return nil, nil, err
	}

	discoveryClient := cacheddiscovery.NewMemCacheClient(internalClient.Discovery())
	discoveryRESTMapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
	kubePluginInitializer := NewPluginInitializer(
		cloudConfig,
		discoveryRESTMapper,
		quotainstall.NewQuotaConfigurationForAdmission(),
	)

	admissionPostStartHook := func(context genericapiserver.PostStartHookContext) error {
		discoveryRESTMapper.Reset()
		go utilwait.Until(discoveryRESTMapper.Reset, 30*time.Second, context.StopCh)
		return nil
	}

	return []admission.PluginInitializer{webhookPluginInitializer, kubePluginInitializer}, admissionPostStartHook, nil
}
