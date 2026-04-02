package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// applicationFinalizer blocks Application deletion until the installer runs helm uninstall
// (same role as operator.odigos.io/odigos-finalizer on the Odigos CR).
const applicationFinalizer = "odigos.io/gcp-marketplace-helm-uninstall"

var applicationGVR = schema.GroupVersionResource{
	Group:    "app.k8s.io",
	Version:  "v1beta1",
	Resource: "applications",
}

// watchApplicationForHelmUninstall watches the Marketplace Application; when it is deleted,
// runs helm uninstall for the Odigos release then removes this finalizer.
func watchApplicationForHelmUninstall(ctx context.Context, k8sConfig *rest.Config, appName, appNamespace, helmNamespace string) {
	dyn, err := dynamic.NewForConfig(k8sConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: dynamic client for Application watch: %v\n", err)
		return
	}

	var mu sync.Mutex
	for ctx.Err() == nil {
		w, err := dyn.Resource(applicationGVR).Namespace(appNamespace).Watch(ctx, metav1.ListOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Application watch failed (retrying): %v\n", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		func() {
			defer w.Stop()
			for e := range w.ResultChan() {
				if ctx.Err() != nil {
					return
				}
				if e.Type == watch.Error {
					continue
				}
				acc, err := meta.Accessor(e.Object)
				if err != nil {
					continue
				}
				if acc.GetName() != appName {
					continue
				}
				if acc.GetDeletionTimestamp() == nil {
					continue
				}
				if !containsString(acc.GetFinalizers(), applicationFinalizer) {
					continue
				}

				mu.Lock()
				err = handleApplicationDeletion(ctx, dyn, k8sConfig, appName, appNamespace, helmNamespace)
				mu.Unlock()
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR: Application deletion hook: %v (will retry on next event)\n", err)
				}
			}
		}()
	}
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
