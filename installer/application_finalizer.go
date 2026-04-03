package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicinformer "k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
)

// applicationFinalizer blocks Application deletion until the installer runs helm uninstall
// (same role as operator.odigos.io/odigos-finalizer on the Odigos CR).
const applicationFinalizer = "odigos.io/gcp-marketplace-helm-uninstall"

var applicationGVR = schema.GroupVersionResource{
	Group:    "app.k8s.io",
	Version:  "v1beta1",
	Resource: "applications",
}

// watchApplicationForHelmUninstall watches the Marketplace Application via a SharedInformer; when it is deleted,
// runs helm uninstall for the Odigos release then removes this finalizer.
//
// sigs.k8s.io/application publishes API types (api/v1beta1) but not a generated clientset/informers package like
// k8s.io/client-go/kubernetes. The usual pattern is dynamic client + dynamicinformer (ListWatch under the hood).
func watchApplicationForHelmUninstall(ctx context.Context, k8sConfig *rest.Config, appName, appNamespace, helmNamespace string) {
	dyn, err := dynamic.NewForConfig(k8sConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: dynamic client for Application informer: %v\n", err)
		return
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 6*time.Minute, appNamespace, nil)
	informer := factory.ForResource(applicationGVR).Informer()

	var mu sync.Mutex
	process := func(obj interface{}) {
		if ctx.Err() != nil {
			return
		}
		app, ok := applicationFromInformerObject(obj)
		if !ok || app.Name != appName {
			return
		}
		if app.DeletionTimestamp == nil || !containsString(app.Finalizers, applicationFinalizer) {
			return
		}
		mu.Lock()
		err := handleApplicationDeletion(ctx, dyn, k8sConfig, appName, appNamespace, helmNamespace)
		mu.Unlock()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Application deletion hook: %v (will retry on resync)\n", err)
		}
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    process,
		UpdateFunc: func(_, newObj interface{}) { process(newObj) },
	})

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		fmt.Fprintf(os.Stderr, "ERROR: Application informer cache sync failed\n")
		return
	}
	fmt.Println("Application informer synced (app.k8s.io/v1beta1)")

	<-ctx.Done()
}

func applicationFromInformerObject(obj interface{}) (*appv1beta1.Application, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return nil, false
		}
		u, ok = tombstone.Obj.(*unstructured.Unstructured)
		if !ok {
			return nil, false
		}
	}
	var app appv1beta1.Application
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &app); err != nil {
		return nil, false
	}
	return &app, true
}

func handleApplicationDeletion(ctx context.Context, dyn dynamic.Interface, k8sConfig *rest.Config, appName, appNamespace, helmNamespace string) error {
	fmt.Printf("Application %s/%s is being deleted; running helm uninstall in namespace %s\n", appNamespace, appName, helmNamespace)
	if err := helmUninstallOdigos(k8sConfig, helmNamespace); err != nil {
		return fmt.Errorf("helm uninstall: %w", err)
	}
	return removeApplicationFinalizer(ctx, dyn, appNamespace, appName)
}

func removeApplicationFinalizer(ctx context.Context, dyn dynamic.Interface, namespace, name string) error {
	ri := dyn.Resource(applicationGVR).Namespace(namespace)
	cur, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get Application: %w", err)
	}
	finalizers := cur.GetFinalizers()
	next := make([]string, 0, len(finalizers))
	for _, f := range finalizers {
		if f != applicationFinalizer {
			next = append(next, f)
		}
	}
	if len(next) == len(finalizers) {
		return nil
	}
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": next,
		},
	})
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch Application finalizers: %w", err)
	}
	fmt.Printf("Removed finalizer %q from Application %s/%s\n", applicationFinalizer, namespace, name)
	return nil
}

func containsString(slice []string, s string) bool {
	for _, x := range slice {
		if x == s {
			return true
		}
	}
	return false
}
