package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"strings"

	"github.com/odigos-io/odigos/cli/pkg/autodetect"
	"github.com/odigos-io/odigos/common"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

//go:embed odigos-chart.tgz
var odigosChartArchive []byte

// restClientGetter implements genericclioptions.RESTClientGetter using an existing rest.Config
// (same pattern as odigos operator/controller).
type restClientGetter struct {
	config    *rest.Config
	namespace string
}

func newRESTClientGetter(config *rest.Config, namespace string) *restClientGetter {
	return &restClientGetter{config: config, namespace: namespace}
}

func (r *restClientGetter) ToRESTConfig() (*rest.Config, error) {
	return r.config, nil
}

func (r *restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	cfg, err := r.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (r *restClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := r.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(dc), nil
}

func (r *restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return &simpleClientConfig{namespace: r.namespace}
}

type simpleClientConfig struct {
	namespace string
}

func (c *simpleClientConfig) RawConfig() (clientcmdapi.Config, error) {
	return clientcmdapi.Config{}, nil
}

func (c *simpleClientConfig) ClientConfig() (*rest.Config, error) {
	return nil, nil
}

func (c *simpleClientConfig) Namespace() (string, bool, error) {
	return c.namespace, true, nil
}

func (c *simpleClientConfig) ConfigAccess() clientcmd.ConfigAccess {
	return nil
}

func loadPackagedChart() error {
	if len(odigosChartArchive) == 0 {
		return fmt.Errorf("embedded odigos-chart.tgz is empty — run `make vendor-helm-chart` before local builds, or build the image with Docker (chart stage)")
	}
	return nil
}

func helmInstallOdigos(config *rest.Config, namespace string, vals map[string]interface{}) error {
	if err := loadPackagedChart(); err != nil {
		return err
	}

	ch, err := loader.LoadArchive(bytes.NewReader(odigosChartArchive))
	if err != nil {
		return fmt.Errorf("load packaged helm chart: %w", err)
	}

	actionConfig := new(action.Configuration)
	debug := func(format string, v ...interface{}) {
		if os.Getenv("ODIGOS_INSTALLER_HELM_DEBUG") != "1" {
			return
		}
		fmt.Printf(format, v...)
	}
	if err := actionConfig.Init(newRESTClientGetter(config, namespace), namespace, "secret", debug); err != nil {
		return err
	}

	result, err := installOrUpgradeHelm(actionConfig, ch, vals, namespace, helmReleaseName, installOrUpgradeOptions{
		CreateNamespace:      false,
		ResetThenReuseValues: true,
	})
	if err != nil {
		return err
	}

	fmt.Println(formatInstallOutcome(result, ch.Metadata.Version))
	return nil
}

func helmUninstallOdigos(config *rest.Config, namespace string) error {
	actionConfig := new(action.Configuration)
	debug := func(format string, v ...interface{}) {
		if os.Getenv("ODIGOS_INSTALLER_HELM_DEBUG") != "1" {
			return
		}
		fmt.Printf(format, v...)
	}
	if err := actionConfig.Init(newRESTClientGetter(config, namespace), namespace, "secret", debug); err != nil {
		return err
	}
	return runHelmUninstall(actionConfig, helmReleaseName)
}

func buildHelmValues(details *autodetect.ClusterDetails, onPremToken, imageTag string, odigosTier common.OdigosTier) map[string]interface{} {
	vals := make(map[string]interface{})

	if onPremToken != "" && onPremToken != "$onPremToken" {
		vals["onPremToken"] = onPremToken
	}

	openshiftEnabled := details != nil && details.Kind == autodetect.KindOpenShift
	vals["openshift"] = map[string]interface{}{"enabled": openshiftEnabled}

	if img := marketplaceImageOverrides(odigosTier == common.CommunityOdigosTier); len(img) > 0 {
		vals["images"] = img
	}

	vals["image"] = map[string]interface{}{
		"tag": imageTag,
	}

	return vals
}

func marketplaceImageOverrides(community bool) map[string]interface{} {
	images := make(map[string]interface{})
	add := func(component, val string) {
		if val == "" || val == "$onPremToken" || strings.HasPrefix(val, "$") {
			return
		}
		images[component] = val
	}

	add("autoscaler", os.Getenv("ODIGOS_AUTOSCALER_IMAGE"))
	add("collector", os.Getenv("ODIGOS_COLLECTOR_IMAGE"))
	add("scheduler", os.Getenv("ODIGOS_SCHEDULER_IMAGE"))
	add("ui", os.Getenv("ODIGOS_UI_IMAGE"))

	if community {
		add("instrumentor", os.Getenv("ODIGOS_INSTRUMENTOR_IMAGE"))
		add("odiglet", os.Getenv("ODIGOS_ODIGLET_IMAGE"))
		if v := os.Getenv("ODIGOS_INIT_CONTAINER_IMAGE"); v != "" {
			add("agents", v)
		}
	} else {
		add("enterprise-instrumentor", os.Getenv("ODIGOS_ENTERPRISE_INSTRUMENTOR_IMAGE"))
		entOdiglet := os.Getenv("ODIGOS_ENTERPRISE_ODIGLET_IMAGE")
		entInit := os.Getenv("ODIGOS_ENTERPRISE_INIT_CONTAINER_IMAGE")
		switch {
		case entInit != "":
			add("enterprise-odiglet", entInit)
			add("enterprise-agents", entInit)
		case entOdiglet != "":
			add("enterprise-odiglet", entOdiglet)
		}
	}

	if len(images) == 0 {
		return nil
	}
	return images
}
