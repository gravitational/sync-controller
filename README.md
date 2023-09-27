# sync-controller

Controller that synchronizes custom resources between Kubernetes clusters.

Teleport Cloud uses Sync Controller in production to operate multi-region deployments of [Teleport](https://goteleport.com).

## Overview

Sync Controller is an importable controller that helps you build multi-cluster management planes using Kubernetes custom resources.

Sync Controller accomplishes this by synchronizing the `spec` and `status` fields of custom resources that live on different clusters.

[image]

Sync Controller runs in a local (e.g., region-specific) cluster and watches a remote (e.g., management plane) cluster for instances of a specified custom resource.

Local resources are created automatically when remote resources appear.
`spec` is synced from remote to local, while `status` is synced in the opposite directly, from local to remote.

Remote resources are generally created and updated by a management-plane controller that is aware of Sync Controller, but they are never reconciled on the remote cluster.
Local resources are created by Sync Controller, and may be reconciled by a controller that is not aware of Sync Controller.

Since the resource `metadata.generation` fields cannot be synced, the local and remote generations are provided via annotations:
```golang
	// AnnotationRemoteGeneration is the annotation placed on remote resources to provide the synced remote generation.
	AnnotationRemoteGeneration = "cloud.teleport.dev/sync-remote-generation"
	// AnnotationLocalGeneration is the annotation placed on remote resources to provide the synced local generation.
	AnnotationLocalGeneration = "cloud.teleport.dev/sync-local-generation"
```

Controllers in the remote cluster can use these additional generation values to understand when `status` is up-to-date with `spec`.
Controllers in the local cluster can safely use `metadata.generation`.

## Details

`controller.Reconciler` has the following additional behavior:
- If the remote resource's namespace does not exist on the local cluster, Reconciler creates it.
- If the remote resource is deleted, Reconciler deletes the entire local namespace (if created by the Reconciler).
- If the local namespace was not created by the Reconciler, the Reconciler will not delete any resources.
- If the local resource is a controlling owner of secrets, and those secrets match pre-defined names,
  then Reconciler updates those secrets in the remote cluster and sets their controller to be the remote resource.
  The remote secrets must exist already, so that RBAC may be restricted to updates.
  This feature should only be used with short-lived secrets, and is not intended to replace the [external-secrets operator](https://external-secrets.io/latest/).
- The remote namespace and resource names may be suffixed with strings that are removed when created locally. 
  If these suffixes are missing, the remote resource is ignored.
  This allows for sharded configurations.

## Usage

```golang
package main

import (
	"log"

	sync "github.com/gravitational/sync-controller/controller"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

func main() {
	// create scheme, contexts, etc.

	remoteConfig, err := config.GetConfigWithContext(remoteContext)
	if err != nil {
		log.Fatal(err)
	}

	remoteCluster, err := cluster.New(remoteConfig, func(options *cluster.Options) {
		options.Scheme = scheme
	})
	if err != nil {
		log.Fatal("Failed to create management cluster")
	}
	localConfig, err = rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}
	mgr, err := ctrl.NewManager(localConfig, ctrl.Options{
		Scheme:             scheme,
		Port:               9443,
	})
	if err != nil {
		log.Fatal(err)
	}
	syncReconciler := &sync.Reconciler{
		Client:                 mgr.GetClient(),
		RemoteClient:           remoteCluster.GetClient(),
		RemoteCache:            remoteCluster.GetCache(),
		Scheme:                 mgr.GetScheme(),
		Resource:               &mypkg.MyCRD{},
		RemoteResourceSuffix:   "-" + region,
		LocalNamespaceSuffix:   "",
		NamespacePrefix:        "my-prefix-",
		LocalSecretNames:       []string{"some-ephemeral-token"},
		LocalPropagationPolicy: client.PropagationPolicy(metav1.DeletePropagationForeground),
		ConcurrentReconciles:   concurrentReconciles,
	}

	if err := syncReconciler.SetupWithManager(mgr); err != nil {
		log.Fatal(err)
	}

	// add other controllers
}

```
