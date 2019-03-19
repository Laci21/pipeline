// Copyright © 2019 Banzai Cloud
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

package cluster

import (
	"strings"

	"github.com/banzaicloud/istio-operator/pkg/apis/istio/v1beta1"
	pConfig "github.com/banzaicloud/pipeline/config"
	"github.com/banzaicloud/pipeline/internal/istio"
	"github.com/banzaicloud/pipeline/pkg/cluster"
	pkgHelm "github.com/banzaicloud/pipeline/pkg/helm"
	"github.com/banzaicloud/pipeline/pkg/k8sclient"
	"github.com/goph/emperror"
	"github.com/spf13/viper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// InstallServiceMeshParams describes InstallServiceMesh posthook params
type InstallServiceMeshParams struct {
	// AutoSidecarInjectNamespaces list of namespaces that will be labelled with istio-injection=enabled
	AutoSidecarInjectNamespaces []string `json:"autoSidecarInjectNamespaces,omitempty"`
	// BypassEgressTraffic prevents Envoy sidecars from intercepting external requests
	BypassEgressTraffic bool `json:"bypassEgressTraffic,omitempty"`
	// EnableMtls signals if mutual TLS is enabled in the service mesh
	EnableMtls bool `json:"mtls,omitempty"`
}

// InstallServiceMesh is a posthook for installing Istio on a cluster
func InstallServiceMesh(cluster CommonCluster, param cluster.PostHookParam) error {
	// Install Istio-operator with helm
	err := installDeployment(cluster, istio.Namespace, pkgHelm.BanzaiRepository+"/istio-operator", "istio-operator", []byte{}, viper.GetString(pConfig.IstioOperatorChartVersion), true)
	if err != nil {
		return emperror.Wrap(err, "installing istio-operator failed")
	}

	// Install Istio by creating a CR for the istio-operator
	var params InstallServiceMeshParams
	err = castToPostHookParam(&param, &params)
	if err != nil {
		return emperror.Wrap(err, "failed to cast posthook param")
	}

	log.Infof("istio params: %#v", params)

	istioConfig := v1beta1.Istio{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Istio",
			APIVersion: "istio.banzaicloud.io/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "istio-config",
			Labels: map[string]string{
				"controller-tools.k8s.io": "1.0",
			},
		},
		Spec: v1beta1.IstioSpec{
			MTLS:                    params.EnableMtls,
			AutoInjectionNamespaces: params.AutoSidecarInjectNamespaces,
		},
	}

	if params.BypassEgressTraffic {
		ipRanges, err := cluster.GetK8sIpv4Cidrs()
		if err != nil {
			log.Warnf("couldn't set included IP ranges in Envoy config, external requests will be intercepted")
		} else {
			istioConfig.Spec.IncludeIPRanges = strings.Join(ipRanges.PodIPRanges, ",") + "," + strings.Join(ipRanges.ServiceClusterIPRanges, ",")
		}
	}

	kubeConfig, err := cluster.GetK8sConfig()
	if err != nil {
		return emperror.Wrap(err, "failed to get kubeconfig")
	}

	config, err := clientcmd.RESTConfigFromKubeConfig(kubeConfig)
	if err != nil {
		return emperror.Wrap(err, "failed to create client from kubeconfig")
	}

	v1beta1.AddToScheme(scheme.Scheme)

	config.ContentConfig.GroupVersion = &schema.GroupVersion{Group: "istio.banzaicloud.io", Version: "v1beta1"}
	config.APIPath = "/apis"
	config.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: scheme.Codecs}
	config.UserAgent = rest.DefaultKubernetesUserAgent()

	restClient, err := rest.RESTClientFor(config)
	if err != nil {
		return err
	}

	err = restClient.Post().
		Namespace(istio.Namespace).
		Resource("istios").
		Body(&istioConfig).
		Do().
		Error()
	if err != nil {
		return emperror.Wrap(err, "failed to create Istio CR")
	}

	client, err := k8sclient.NewClientFromKubeConfig(kubeConfig)
	if err != nil {
		return emperror.Wrap(err, "failed to create client from kubeconfig")
	}
	if cluster.GetMonitoring() {
		err = istio.AddPrometheusTargets(log, client)
		if err != nil {
			return emperror.Wrap(err, "failed to add prometheus targets")
		}
		err = istio.AddGrafanaDashboards(log, client)
		if err != nil {
			return emperror.Wrap(err, "failed to add grafana dashboards")
		}
	}

	cluster.SetServiceMesh(true)
	return nil
}
