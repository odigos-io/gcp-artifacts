package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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

// applicationResyncPeriod matches the prior dynamic informer resync (retry failed uninstall hook).
const applicationResyncPeriod = 6 * time.Minute

// watchApplicationForHelmUninstall watches the Marketplace Application via controller-runtime cache;
// when it is deleted, runs helm uninstall for the Odigos release then removes this finalizer.
//
// sigs.k8s.io/application publishes API types (api/v1beta1) but not a generated clientset/informers
// package like k8s.io/client-go/kubernetes; controller-runtime cache is the usual typed alternative
// to dynamic client + dynamicinformer.
//
// inFlightFinalizer, when non-nil, has Add(1) before and Done() after each handleApplicationDeletion so the process
// can wait on shutdown (SIGTERM) for helm uninstall + finalizer removal to finish before exiting.
func watchApplicationForHelmUninstall(ctx context.Context, k8sConfig *rest.Config, appName, appNamespace, helmNamespace string, inFlightFinalizer *sync.WaitGroup) {
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
		if inFlightFinalizer != nil {
			inFlightFinalizer.Add(1)
			defer inFlightFinalizer.Done()
		}
		mu.Lock()
		err := handleApplicationDeletion(ctx, appClient, k8sConfig, appName, appNamespace, helmNamespace)
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
	fmt.Println("Application cache synced (app.k8s.io/v1beta1)")

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

func handleApplicationDeletion(ctx context.Context, appCli crclient.Client, k8sConfig *rest.Config, appName, appNamespace, helmNamespace string) error {
	fmt.Printf("Application %s/%s is being deleted; running helm uninstall in namespace %s\n", appNamespace, appName, helmNamespace)
	if err := helmUninstallOdigos(k8sConfig, helmNamespace); err != nil {
		return fmt.Errorf("helm uninstall: %w", err)
	}
	return removeApplicationFinalizer(ctx, appCli, appNamespace, appName)
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
