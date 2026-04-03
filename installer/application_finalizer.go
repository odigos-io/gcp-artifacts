package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// applicationFinalizer blocks Application deletion until the installer runs helm uninstall
// (same role as operator.odigos.io/odigos-finalizer on the Odigos CR).
const applicationFinalizer = "odigos.io/gcp-marketplace-helm-uninstall"

// applicationResyncPeriod is the controller-runtime cache resync (retries failed uninstall from the watch path).
const applicationResyncPeriod = 6 * time.Minute

func applicationClient(k8sConfig *rest.Config) (crclient.Client, error) {
	scheme := runtime.NewScheme()
	utilruntime.Must(appv1beta1.AddToScheme(scheme))
	return crclient.New(k8sConfig, crclient.Options{Scheme: scheme})
}

// applicationOwnerRefFromDeployment returns the ownerReference GCP Marketplace sets on the installer Deployment
// pointing at the Application CR (dependent Deployment → owner Application).
func applicationOwnerRefFromDeployment(dep *appsv1.Deployment) *metav1.OwnerReference {
	for i := range dep.OwnerReferences {
		ref := &dep.OwnerReferences[i]
		if ref.Kind != "Application" {
			continue
		}
		if strings.HasPrefix(ref.APIVersion, "app.k8s.io/") {
			return ref
		}
	}
	return nil
}

func processApplicationIfStuckOnShutdown(ctx context.Context, appClient crclient.Client, k8sConfig *rest.Config, app *appv1beta1.Application, helmNamespace string) {
	if !controllerutil.ContainsFinalizer(app, applicationFinalizer) {
		fmt.Printf("Shutdown: Application %s/%s has no finalizer %q; skipping\n", app.Namespace, app.Name, applicationFinalizer)
		return
	}
	if err := handleApplicationDeletion(ctx, appClient, k8sConfig, app, helmNamespace); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: shutdown handler for %s/%s: %v\n", app.Namespace, app.Name, err)
	}
}

// watchApplicationForHelmUninstall watches the Marketplace Application via controller-runtime cache. When the
// Application is deleted (e.g. kubectl delete application), runs helm uninstall and removes applicationFinalizer.
func watchApplicationForHelmUninstall(ctx context.Context, k8sConfig *rest.Config, appName, appNamespace, helmNamespace string) {
	appScheme := runtime.NewScheme()
	utilruntime.Must(appv1beta1.AddToScheme(appScheme))

	appCache, err := crcache.New(k8sConfig, crcache.Options{
		Scheme: appScheme,
		DefaultNamespaces: map[string]crcache.Config{
			appNamespace: {},
		},
		SyncPeriod: ptr.To(applicationResyncPeriod),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: controller-runtime cache for Application: %v\n", err)
		return
	}

	appClient, err := crclient.New(k8sConfig, crclient.Options{Scheme: appScheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: controller-runtime client for Application: %v\n", err)
		return
	}

	informer, err := appCache.GetInformer(ctx, &appv1beta1.Application{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Application informer: %v\n", err)
		return
	}

	var mu sync.Mutex
	process := func(obj interface{}) {
		if ctx.Err() != nil {
			return
		}
		app, ok := applicationFromInformerObject(obj)
		if !ok || app.Name != appName {
			return
		}
		if app.DeletionTimestamp == nil || !controllerutil.ContainsFinalizer(app, applicationFinalizer) {
			return
		}
		mu.Lock()
		err := handleApplicationDeletion(ctx, appClient, k8sConfig, app, helmNamespace)
		mu.Unlock()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Application deletion hook: %v (will retry on resync)\n", err)
		}
	}

	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    process,
		UpdateFunc: func(_, newObj interface{}) { process(newObj) },
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Application informer handler: %v\n", err)
		return
	}

	go func() {
		if err := appCache.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Application cache: %v\n", err)
		}
	}()

	if !appCache.WaitForCacheSync(ctx) {
		fmt.Fprintf(os.Stderr, "ERROR: Application cache sync failed\n")
		return
	}
	fmt.Println("Application informer ready (app.k8s.io/v1beta1); delete Application triggers helm uninstall")

	<-ctx.Done()
}

func applicationFromInformerObject(obj interface{}) (*appv1beta1.Application, bool) {
	if app, ok := obj.(*appv1beta1.Application); ok {
		return app, true
	}
	tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown)
	if !ok {
		return nil, false
	}
	app, ok := tombstone.Obj.(*appv1beta1.Application)
	if !ok {
		return nil, false
	}
	return app, true
}

// processApplicationsWithOdigosFinalizerOnShutdown resolves the Marketplace Application for SIGTERM cleanup:
// prefer installer Deployment (ODIGOS_INSTALLER_NAMESPACE / ODIGOS_INSTALLER_NAME) and its Application
// ownerReference; if the Deployment is already gone (common during namespace deletion), fall back to Application
// with the same name/namespace as the installer (matches application.yaml.template / manifests).
func processApplicationsWithOdigosFinalizerOnShutdown(ctx context.Context, k8sConfig *rest.Config, clientset kubernetes.Interface, deployNamespace, deployName, helmNamespace string) error {
	appClient, err := applicationClient(k8sConfig)
	if err != nil {
		return fmt.Errorf("application client: %w", err)
	}

	if deployName == "" || deployNamespace == "" {
		fmt.Fprintf(os.Stderr, "WARN: shutdown skip: installer Deployment name or namespace empty\n")
		return nil
	}

	var appKey crclient.ObjectKey
	dep, depErr := clientset.AppsV1().Deployments(deployNamespace).Get(ctx, deployName, metav1.GetOptions{})
	switch {
	case depErr == nil:
		fmt.Printf("Shutdown: installer Deployment %s/%s (uid=%s)\n", dep.Namespace, dep.Name, dep.UID)
		if ref := applicationOwnerRefFromDeployment(dep); ref != nil {
			appKey = crclient.ObjectKey{Namespace: dep.Namespace, Name: ref.Name}
		} else {
			fmt.Fprintf(os.Stderr, "WARN: Deployment %s/%s has no app.k8s.io Application ownerReference; trying Application %s/%s\n", dep.Namespace, dep.Name, deployNamespace, deployName)
			appKey = crclient.ObjectKey{Namespace: deployNamespace, Name: deployName}
		}
	case apierrors.IsNotFound(depErr):
		fmt.Printf("Shutdown: installer Deployment %s/%s not found (namespace teardown?); trying Application %s/%s\n", deployNamespace, deployName, deployNamespace, deployName)
		appKey = crclient.ObjectKey{Namespace: deployNamespace, Name: deployName}
	default:
		fmt.Fprintf(os.Stderr, "WARN: shutdown: get installer Deployment %s/%s: %v\n", deployNamespace, deployName, depErr)
		return nil
	}

	var app appv1beta1.Application
	if err := appClient.Get(ctx, appKey, &app); err != nil {
		if apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "WARN: shutdown: Application %s/%s not found\n", appKey.Namespace, appKey.Name)
			return nil
		}
		return fmt.Errorf("get Application %s/%s: %w", appKey.Namespace, appKey.Name, err)
	}
	if depErr == nil {
		if ref := applicationOwnerRefFromDeployment(dep); ref != nil && app.UID != ref.UID {
			fmt.Fprintf(os.Stderr, "WARN: Application %s/%s UID %s != owner ref UID %s\n", app.Namespace, app.Name, app.UID, ref.UID)
		}
	}
	processApplicationIfStuckOnShutdown(ctx, appClient, k8sConfig, &app, helmNamespace)
	return nil
}

func handleApplicationDeletion(ctx context.Context, appCli crclient.Client, k8sConfig *rest.Config, app *appv1beta1.Application, helmNamespace string) error {
	if app == nil {
		return fmt.Errorf("application is nil")
	}
	if app.DeletionTimestamp == nil {
		fmt.Printf("Application %s/%s has no deletionTimestamp; skipping helm uninstall and finalizer removal\n", app.Namespace, app.Name)
		return nil
	}
	fmt.Printf("Application %s/%s is deleting; running helm uninstall in namespace %s\n", app.Namespace, app.Name, helmNamespace)
	if err := helmUninstallOdigos(k8sConfig, helmNamespace); err != nil {
		return fmt.Errorf("helm uninstall: %w", err)
	}
	return removeApplicationFinalizer(ctx, appCli, app.Namespace, app.Name)
}

func removeApplicationFinalizer(ctx context.Context, appClient crclient.Client, namespace, name string) error {
	var app appv1beta1.Application
	key := crclient.ObjectKey{Namespace: namespace, Name: name}
	if err := appClient.Get(ctx, key, &app); err != nil {
		return fmt.Errorf("get Application: %w", err)
	}
	orig := app.DeepCopy()
	if !controllerutil.RemoveFinalizer(&app, applicationFinalizer) {
		return nil
	}
	if err := appClient.Patch(ctx, &app, crclient.MergeFrom(orig)); err != nil {
		return fmt.Errorf("patch Application finalizers: %w", err)
	}
	fmt.Printf("Removed finalizer %q from Application %s/%s\n", applicationFinalizer, namespace, name)
	return nil
}
