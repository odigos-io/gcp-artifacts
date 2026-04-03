package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/odigos-io/odigos/cli/pkg/autodetect"
	"github.com/odigos-io/odigos/cli/pkg/kube"
	"github.com/odigos-io/odigos/common"
	"github.com/odigos-io/odigos/common/consts"
)

const odigletDaemonSetName = "odiglet"

// shutdownDrainTimeout is how long we wait after SIGTERM for in-flight Application deletion
// (helm uninstall + finalizer patch) before exiting. Must be less than the pod
// terminationGracePeriodSeconds (see manifests.yaml.template).
func shutdownDrainTimeout() time.Duration {
	const defaultDrain = 270 * time.Second
	s := os.Getenv("ODIGOS_INSTALLER_SHUTDOWN_DRAIN")
	if s == "" {
		return defaultDrain
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return defaultDrain
	}
	return d
}

func waitFinalizerDrain(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		fmt.Println("Application uninstall / finalizer work completed")
	case <-time.After(timeout):
		fmt.Fprintf(os.Stderr, "WARN: shutdown drain timed out after %v; Application CR may retain a finalizer\n", timeout)
	}
}

func main() {
	ctx := context.Background()

	fmt.Println("Starting Odigos installer")

	fmt.Println("Getting k8s config")
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: unable to get k8s config: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Creating k8s clientset")
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to create clientset: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Creating kube client")
	kubeClient := &kube.Client{
		Interface: clientset,
		Clientset: clientset,
		Config:    k8sConfig,
	}

	details := autodetect.GetK8SClusterDetails(ctx, "", "", kubeClient)

	ns := os.Getenv("ODIGOS_NAMESPACE")
	onPremToken := os.Getenv("ODIGOS_ON_PREM_TOKEN")
	odigosTier := common.CommunityOdigosTier
	if onPremToken != "" && onPremToken != "$onPremToken" {
		odigosTier = common.OnPremOdigosTier
	}

	version := os.Getenv(consts.OdigosVersionEnvVarName)
	if version == "" {
		version = os.Getenv("ODIGOS_VERSION")
	}
	if version == "" {
		fmt.Fprintf(os.Stderr, "ERROR: Odigos version not set (%s or ODIGOS_VERSION)\n", consts.OdigosVersionEnvVarName)
		os.Exit(1)
	}
	imageTag := version
	if !strings.HasPrefix(imageTag, "v") {
		imageTag = "v" + imageTag
	}

	vals := buildHelmValues(details, onPremToken, imageTag, odigosTier)

	odigosInstallerName := os.Getenv("ODIGOS_INSTALLER_NAME")
	odigosInstallerNamespace := os.Getenv("ODIGOS_INSTALLER_NAMESPACE")

	fmt.Println("Installing Odigos via Helm chart")
	if err := helmInstallOdigos(k8sConfig, ns, vals); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: helm install failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Odigos installation completed successfully")

	// Separate from the root context so SIGTERM does not cancel API calls mid-flight during
	// helm uninstall + Application finalizer removal (namespace teardown deletes the pod quickly).
	watchCtx, watchCancel := context.WithCancel(context.Background())
	var finalizerDrain sync.WaitGroup

	if odigosInstallerName != "" && odigosInstallerNamespace != "" && ns != "" {
		fmt.Printf("Watching Application %s/%s for deletion (helm uninstall finalizer)\n", odigosInstallerNamespace, odigosInstallerName)
		go watchApplicationForHelmUninstall(watchCtx, k8sConfig, odigosInstallerName, odigosInstallerNamespace, ns, &finalizerDrain)
	}

	daemonCtx, daemonCancel := context.WithCancel(context.Background())
	if ns != "" {
		fmt.Println("Starting odiglet daemonset watcher")
		watchOdigletDaemonSet(daemonCtx, clientset, ns)
	}

	fmt.Println("Installer running, waiting for shutdown signal...")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("Shutdown signal received; stopping side watchers and draining Application finalizer work if any...")
	daemonCancel()
	waitFinalizerDrain(&finalizerDrain, shutdownDrainTimeout())
	watchCancel()
	fmt.Println("Exiting")
}

func reportUsage(ds *appsv1.DaemonSet) {
	replicas := ds.Status.DesiredNumberScheduled
	fmt.Printf("Reporting usage: installed_nodes=%d\n", replicas)

	agentPort := os.Getenv("AGENT_LOCAL_PORT")
	if agentPort == "" {
		agentPort = "4567"
	}

	report := map[string]interface{}{
		"name": "installed_nodes",
		"value": map[string]interface{}{
			"int64Value": int64(replicas),
		},
		"startTime": time.Now().UTC().Format(time.RFC3339),
		"endTime":   time.Now().UTC().Format(time.RFC3339),
	}

	reportJSON, err := json.Marshal(report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to marshal usage report: %v\n", err)
		return
	}

	url := fmt.Sprintf("http://localhost:%s/report", agentPort)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(reportJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to send usage report to agent: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "ERROR: agent returned status %d for usage report\n", resp.StatusCode)
		return
	}

	fmt.Println("Usage report sent successfully to billing agent")
}

func watchOdigletDaemonSet(ctx context.Context, clientset *kubernetes.Clientset, namespace string) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		time.Minute*5,
		informers.WithNamespace(namespace),
	)

	daemonSetInformer := factory.Apps().V1().DaemonSets().Informer()

	daemonSetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ds := obj.(*appsv1.DaemonSet)
			if ds.Name == odigletDaemonSetName {
				fmt.Printf("Odiglet DaemonSet added: %s/%s\n", ds.Namespace, ds.Name)
				reportUsage(ds)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			ds := newObj.(*appsv1.DaemonSet)
			if ds.Name == odigletDaemonSetName {
				fmt.Printf("Odiglet DaemonSet updated: %s/%s\n", ds.Namespace, ds.Name)
				reportUsage(ds)
			}
		},
		DeleteFunc: func(obj interface{}) {
			ds := obj.(*appsv1.DaemonSet)
			if ds.Name == odigletDaemonSetName {
				fmt.Printf("Odiglet DaemonSet deleted: %s/%s\n", ds.Namespace, ds.Name)
				reportUsage(ds)
			}
		},
	})

	stopCh := make(chan struct{})
	go factory.Start(stopCh)
	go func() {
		<-ctx.Done()
		close(stopCh)
	}()

	if !cache.WaitForCacheSync(ctx.Done(), daemonSetInformer.HasSynced) {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to sync cache for DaemonSet informer\n")
		return
	}

	fmt.Println("Odiglet DaemonSet watcher started successfully")

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ds, err := clientset.AppsV1().DaemonSets(namespace).Get(ctx, odigletDaemonSetName, metav1.GetOptions{})
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR: Failed to get odiglet DaemonSet for periodic reporting: %v\n", err)
					continue
				}
				fmt.Println("Periodic usage report (60s interval)")
				reportUsage(ds)
			}
		}
	}()
}
