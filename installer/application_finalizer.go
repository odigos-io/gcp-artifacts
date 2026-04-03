package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

// namespaceSuggestsSIGTERMReasonTeardown returns true when the Application's namespace is terminating or already
// gone. During namespace deletion the API server often SIGTERMs pods before the Application object has
// metadata.deletionTimestamp set, so we use Namespace.Status.Phase as an additional signal on shutdown only.
func namespaceSuggestsSIGTERMReasonTeardown(ctx context.Context, clientset kubernetes.Interface, namespace string) (bool, string) {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, "Namespace resource not found (likely removed during teardown)"
		}
		fmt.Fprintf(os.Stderr, "Shutdown: could not GET Namespace %q for phase check: %v\n", namespace, err)
		return false, ""
	}
	switch ns.Status.Phase {
	case corev1.NamespaceTerminating:
		return true, fmt.Sprintf("Namespace %q phase=Terminating", namespace)
	default:
		return false, fmt.Sprintf("Namespace %q phase=%s", namespace, ns.Status.Phase)
	}
}

func processApplicationIfStuckOnShutdown(ctx context.Context, appClient crclient.Client, k8sConfig *rest.Config, clientset kubernetes.Interface, app *appv1beta1.Application, helmNamespace string, installerDeploymentDeleting bool) {
	hasFin := controllerutil.ContainsFinalizer(app, applicationFinalizer)
	var delTS string
	if app.DeletionTimestamp != nil {
		delTS = app.DeletionTimestamp.String()
	} else {
		delTS = "<nil>"
	}
	fmt.Printf("Shutdown: candidate Application %s/%s uid=%s deletionTimestamp=%s hasFinalizer=%v resourceVersion=%s\n",
		app.Namespace, app.Name, app.UID, delTS, hasFin, app.ResourceVersion)
	if !hasFin {
		fmt.Printf("Shutdown: Application %s/%s has no finalizer %q; nothing to do\n", app.Namespace, app.Name, applicationFinalizer)
		return
	}

	appDeleting := app.DeletionTimestamp != nil
	nsTeardown, nsDetail := namespaceSuggestsSIGTERMReasonTeardown(ctx, clientset, app.Namespace)
	fmt.Printf("Shutdown: namespace teardown cue: %v (%s); installer Deployment deleting=%v\n", nsTeardown, nsDetail, installerDeploymentDeleting)

	if !appDeleting && !nsTeardown && !installerDeploymentDeleting {
		fmt.Printf("Shutdown: skip helm/finalizer — Application not deleting, namespace not terminating, installer Deployment not deleting (e.g. rollout restart)\n")
		return
	}
	if !appDeleting && (nsTeardown || installerDeploymentDeleting) {
		reason := nsDetail
		if installerDeploymentDeleting && !nsTeardown {
			reason = "installer Deployment has deletionTimestamp"
		} else if installerDeploymentDeleting && nsTeardown {
			reason = nsDetail + "; installer Deployment also deleting"
		}
		fmt.Printf("Shutdown: proceeding without Application deletionTimestamp (%s)\n", reason)
	}

	allowWithoutDeletionTimestamp := !appDeleting && (nsTeardown || installerDeploymentDeleting)
	if err := handleApplicationDeletion(ctx, appClient, k8sConfig, app, helmNamespace, allowWithoutDeletionTimestamp); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: shutdown handler for %s/%s: %v\n", app.Namespace, app.Name, err)
	} else {
		fmt.Printf("Shutdown: handleApplicationDeletion finished OK for %s/%s\n", app.Namespace, app.Name)
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
		err := handleApplicationDeletion(ctx, appClient, k8sConfig, app, helmNamespace, false)
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
		fmt.Fprintf(os.Stderr, "Shutdown: ABORT empty installer identity (deployNamespace=%q deployName=%q)\n", deployNamespace, deployName)
		return nil
	}

	fmt.Printf("Shutdown: begin helmNamespace=%q lookup installer Deployment %s/%s\n", helmNamespace, deployNamespace, deployName)

	var appKey crclient.ObjectKey
	dep, depErr := clientset.AppsV1().Deployments(deployNamespace).Get(ctx, deployName, metav1.GetOptions{})
	switch {
	case depErr == nil:
		fmt.Printf("Shutdown: got Deployment %s/%s uid=%s ownerRefs=%d\n", dep.Namespace, dep.Name, dep.UID, len(dep.OwnerReferences))
		if ref := applicationOwnerRefFromDeployment(dep); ref != nil {
			fmt.Printf("Shutdown: using Application from Deployment ownerRef name=%s uid=%s apiVersion=%s\n", ref.Name, ref.UID, ref.APIVersion)
			appKey = crclient.ObjectKey{Namespace: dep.Namespace, Name: ref.Name}
		} else {
			fmt.Fprintf(os.Stderr, "Shutdown: Deployment %s/%s has no app.k8s.io Application ownerReference; fallback Application %s/%s\n", dep.Namespace, dep.Name, deployNamespace, deployName)
			appKey = crclient.ObjectKey{Namespace: deployNamespace, Name: deployName}
		}
	case apierrors.IsNotFound(depErr):
		fmt.Printf("Shutdown: Deployment %s/%s IsNotFound — fallback Application %s/%s\n", deployNamespace, deployName, deployNamespace, deployName)
		appKey = crclient.ObjectKey{Namespace: deployNamespace, Name: deployName}
	default:
		fmt.Fprintf(os.Stderr, "Shutdown: ABORT get Deployment %s/%s: %T %v\n", deployNamespace, deployName, depErr, depErr)
		return nil
	}

	fmt.Printf("Shutdown: GET Application %s/%s\n", appKey.Namespace, appKey.Name)
	var app appv1beta1.Application
	if err := appClient.Get(ctx, appKey, &app); err != nil {
		if apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "Shutdown: ABORT Application %s/%s IsNotFound (Deployment path may be wrong or Application already removed)\n", appKey.Namespace, appKey.Name)
			return nil
		}
		return fmt.Errorf("get Application %s/%s: %w", appKey.Namespace, appKey.Name, err)
	}
	fmt.Printf("Shutdown: got Application %s/%s uid=%s\n", app.Namespace, app.Name, app.UID)
	installerDepDeleting := false
	if depErr == nil {
		installerDepDeleting = dep.DeletionTimestamp != nil
		if ref := applicationOwnerRefFromDeployment(dep); ref != nil && app.UID != ref.UID {
			fmt.Fprintf(os.Stderr, "Shutdown: WARN Application uid %s != ownerRef uid %s\n", app.UID, ref.UID)
		}
	}
	processApplicationIfStuckOnShutdown(ctx, appClient, k8sConfig, clientset, &app, helmNamespace, installerDepDeleting)
	fmt.Printf("Shutdown: end processApplicationsWithOdigosFinalizerOnShutdown for Application %s/%s\n", appKey.Namespace, appKey.Name)
	return nil
}

func handleApplicationDeletion(ctx context.Context, appCli crclient.Client, k8sConfig *rest.Config, app *appv1beta1.Application, helmNamespace string, allowWithoutDeletionTimestamp bool) error {
	if app == nil {
		return fmt.Errorf("application is nil")
	}
	if app.DeletionTimestamp == nil && !allowWithoutDeletionTimestamp {
		fmt.Printf("Application hook: %s/%s has no deletionTimestamp; skip helm uninstall and finalizer removal\n", app.Namespace, app.Name)
		return nil
	}
	if app.DeletionTimestamp == nil && allowWithoutDeletionTimestamp {
		fmt.Printf("Application hook: %s/%s shutdown namespace-teardown path (no Application deletionTimestamp yet) — helm uninstall in namespace %q\n", app.Namespace, app.Name, helmNamespace)
	} else {
		fmt.Printf("Application hook: %s/%s deleting since %v — helm uninstall in namespace %q\n", app.Namespace, app.Name, app.DeletionTimestamp.Time, helmNamespace)
	}
	if err := helmUninstallOdigos(k8sConfig, helmNamespace); err != nil {
		return fmt.Errorf("helm uninstall: %w", err)
	}
	fmt.Printf("Application hook: helm uninstall ok — patch finalizer on Application %s/%s\n", app.Namespace, app.Name)
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
		fmt.Printf("Application hook: finalizer %q already absent on %s/%s after refresh GET; no patch\n", applicationFinalizer, namespace, name)
		return nil
	}
	if err := appClient.Patch(ctx, &app, crclient.MergeFrom(orig)); err != nil {
		return fmt.Errorf("patch Application finalizers: %w", err)
	}
	fmt.Printf("Application hook: removed finalizer %q from Application %s/%s\n", applicationFinalizer, namespace, name)
	return nil
}
