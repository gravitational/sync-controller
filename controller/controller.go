// Package synccontroller provides a Kubernetes reconciler that syncs resources of a specified GVK from a remote Kubernetes cluster to a local Kubernetes cluster.
//
// This can be used to create remote management planes, where many local clusters listen to a management cluster for new resources,
// create their own copies when they appear remotely, and upload any status that results from local reconciliation.
//
// For a given resource,
// - The resource on the remote cluster is the source-of-truth, and Reconciler copies its spec to the resource in the local cluster.
// - The resource on the local cluster is created automatically if it does not exist, and Reconciler copies its status to the resource in the remote cluster.
// - If the remote resource's namespace does not exist on the local cluster, Reconciler creates it.
// - If the remote resource is deleted, Reconciler deletes the entire local namespace (if created by sync-controller).
// - If the local namespace was not created by sync-controller, Reconciler will not delete any resources.
//
// Additionally, if secrets controlled by the local resource are created, and they match pre-defined names,
// Reconciler updates those secrets in the remote cluster and sets their controller to be the remote resource.
// The remote secrets must exist already, so that RBAC may be restricted to updates.
// This feature should only be used with short-lived secrets.
//
// The remote namespace and resource names may be suffixed with strings that are removed when created locally.
// If these suffixes are missing, the remote resource is ignored.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/gravitational/trace"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// logKeyChild is a non-reconciled object name, when logged.
	logKeyChild = "childName"
	// logKeyOwner is an owner, when logged.
	logKeyOwner = "ownerName"
	// logKeyRemote is the remote version of the sync resource being reconciled, when logged.
	logKeyRemote = "remoteName"
	// logKeyLocal is the local version of the sync resource being reconciled, when logged.
	logKeyLocal = "localName"
	// logKeyRemoteNamespace is the remote version of the namespace of the sync resource being reconciled, when logged.
	logKeyRemoteNamespace = "remoteNamespace"
	// logKeyLocalNamespace is the local version of the namespace of the sync resource being reconciled, when logged.
	logKeyLocalNamespace = "localNamespace"
	// logKeyRemoteGeneration is the remote generation, when logged.
	logKeyRemoteGeneration = "remoteGeneration"
	// logKeyLocalGeneration is the local generation, when logged.
	logKeyLocalGeneration = "localGeneration"

	// fieldStatus is the struct field name containing the sync resource's status struct.
	fieldStatus = "Status"
	// fieldSpec is the struct field name containing the sync resource's spec struct.
	fieldSpec = "Spec"

	// AnnotationPaused is the annotation used to pause the controller (on either local or remote resource).
	AnnotationPaused = "cloud.teleport.dev/paused"
	// AnnotationNamespaceOwner is the annotation placed on namespaces to know who created them.
	AnnotationNamespaceOwner = "cloud.teleport.dev/sync-owner"
	// AnnotationLastUploadedHash is the annotation placed on resources to represent their last uploaded values.
	AnnotationLastUploadedHash = "cloud.teleport.dev/last-uploaded-hash"
	// AnnotationRemoteGeneration is the annotation placed on remote resources to provide the synced remote generation.
	AnnotationRemoteGeneration = "cloud.teleport.dev/sync-remote-generation"
	// AnnotationLocalGeneration is the annotation placed on remote resources to provide the synced local generation.
	AnnotationLocalGeneration = "cloud.teleport.dev/sync-local-generation"

	// FinalizerRemote is the finalizer that holds remote deletion on local deletion.
	// FinalizerRemote is added by sync-controller to claim the remote resource.
	FinalizerRemote = "cloud.teleport.dev/sync-local"
)

// Reconciler syncs objects matching the GVK of Resource (and controlled secrets) between clusters.
// Reconciler does not support runtime configuration of the synced objects,
// but the synced object may be provided at compile-time in Resource.
type Reconciler struct {
	// Client is the local client, where the resource is created
	client.Client
	// RemoteClient is the remote client, where spec is copied from
	RemoteClient client.Client
	// RemoteCache is the remote cluster cache
	RemoteCache cache.Cache
	// Scheme is the Kubernetes scheme
	Scheme *runtime.Scheme
	// Resource specifies the GVK to sync. MUST have Spec and Status struct fields.
	Resource client.Object
	// RemoteResourceSuffix specifies a suffix to append to all remote resources.
	// Sync-controller ignores remote resources without this suffix.
	// It is removed from the locally created sync resource.
	RemoteResourceSuffix string
	// LocalNamespaceSuffix specifies a suffix to append to the local namespace.
	// Sync-controller ignores local namespaces without this suffix.
	// It is removed from the locally created sync namespace.
	LocalNamespaceSuffix string
	// NamespacePrefix specifies a prefix to require for all namespaces.
	// Sync-controller ignores namespaces without this prefix.
	NamespacePrefix string
	// LocalSecretNames specifies secret namespaces to sync from the local cluster
	// to the remote cluster. For security,
	// - Local secrets must be controlled by the locally created resource.
	// - Remote secrets created will be controlled by the remote resource.
	// - Secrets are only synced after status is successfully synced.
	LocalSecretNames []string
	// LocalPropagationPolicy determines how the local resource will be deleted.
	// Defaults to Foreground if not set.
	// See client.PropagationPolicy for details.
	LocalPropagationPolicy client.PropagationPolicy
	// ConcurrentReconciles sets the number of concurrent reconciles.
	ConcurrentReconciles int
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	// Extra check
	if !strings.HasPrefix(req.Namespace, r.NamespacePrefix) {
		return ctrl.Result{}, nil
	}

	remote := r.Resource.DeepCopyObject().(client.Object)
	remote.SetName(req.Name + r.RemoteResourceSuffix)
	remote.SetNamespace(req.Namespace)

	local := r.Resource.DeepCopyObject().(client.Object)
	local.SetName(req.Name)
	local.SetNamespace(req.Namespace + r.LocalNamespaceSuffix)
	localNs := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: local.GetNamespace()},
	}

	ctx = log.IntoContext(ctx, log.FromContext(ctx).
		WithCallDepth(0).
		WithValues(
			logKeyRemote, remote.GetName(),
			logKeyRemoteNamespace, remote.GetNamespace(),
			logKeyLocal, local.GetName(),
			logKeyLocalNamespace, local.GetNamespace(),
		),
	)
	log := log.FromContext(ctx)

	// Get remote, local, and local namespace.
	// To ensure synchronization, these should not be modified.
	// Otherwise, we could, e.g., overwrite pause annotations.
	err = r.RemoteClient.Get(ctx, client.ObjectKeyFromObject(remote), remote)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "(remote) unable to fetch resource")
		return ctrl.Result{}, trace.Wrap(err)
	}
	err = r.Get(ctx, client.ObjectKeyFromObject(localNs), localNs)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "(local) unable to fetch namespace")
		return ctrl.Result{}, trace.Wrap(err)
	}
	err = r.Get(ctx, client.ObjectKeyFromObject(local), local)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "(local) unable to fetch resource")
		return ctrl.Result{}, trace.Wrap(err)
	}

	// Check pause annotations
	if a := local.GetAnnotations(); a[AnnotationPaused] == "true" {
		log.V(1).Info("(local) paused via annotation")
		return ctrl.Result{}, nil
	}
	if a := remote.GetAnnotations(); a[AnnotationPaused] == "true" {
		log.V(1).Info("(remote) paused via annotation")
		return ctrl.Result{}, nil
	}

	// If remote is missing or being deleted, delete local namespace or resource and quit.
	// The namespace is only deleted if its owning resource is being deleted.
	if creationTime(remote).IsZero() ||
		!remote.GetDeletionTimestamp().IsZero() {

		// Delete namespace if present and created for this resource.
		// Otherwise, just delete the resource if it exists.
		if !creationTime(localNs).IsZero() &&
			localNs.Annotations[AnnotationNamespaceOwner] == local.GetName() {

			if localNs.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, localNs); err != nil {
					log.Error(err, "(local) unable to delete namespace", logKeyChild, localNs.GetName())
					return ctrl.Result{}, trace.Wrap(err)
				}
				log.Info("(local) deleted namespace", logKeyChild, localNs.GetName())
			}
			return ctrl.Result{}, nil // requeue when deleted
		}

		propPolicy := client.PropagationPolicy(metav1.DeletePropagationForeground)
		if r.LocalPropagationPolicy != "" {
			propPolicy = r.LocalPropagationPolicy
		}
		if !creationTime(local).IsZero() {
			if local.GetDeletionTimestamp().IsZero() {
				if err := r.Delete(ctx, local, propPolicy); err != nil {
					log.Error(err, "(local) unable to delete resource")
					return ctrl.Result{}, trace.Wrap(err)
				}
				log.Info("(local) deleted resource")
			}
			return ctrl.Result{}, nil // requeue when deleted
		}

		if !remote.GetDeletionTimestamp().IsZero() {
			ok, err := r.removeFinalizer(ctx, remote)
			if err != nil {
				return ctrl.Result{}, err
			}
			if ok {
				// requeue to ensure we re-check pause annotations / other changes.
				return ctrl.Result{}, nil
			}
		}

		// always requeue if remote is going away
		return ctrl.Result{}, nil
	}

	// At this point, we know remote exists and is not being deleted.

	ok, err := r.addFinalizer(ctx, remote)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ok {
		// Requeue to ensure we re-check pause annotations / other changes.
		return ctrl.Result{}, nil
	}

	// Get secrets
	secrets, err := r.getSecrets(ctx, local)
	if err != nil {
		defer setError(trace.Wrap(err), &err)
	}

	// Upload status and secrets
	if err := r.syncRemote(ctx, local, remote); err != nil {
		return ctrl.Result{}, trace.Wrap(err)
	}

	// Upload any requested secrets that local owns.
	if err := r.updateSecrets(ctx, remote, secrets); err != nil {
		defer setError(trace.Wrap(err), &err)
	}

	// If local is being deleted, its spec is immutable and we cannot proceed.
	// Remote still exists, so we'll need to create eventually.
	if !local.GetDeletionTimestamp().IsZero() {
		log.V(1).Info("(local) pending delete, waiting to recreate")
		return ctrl.Result{}, nil
	}

	// Download spec. After this method, both local and remote are updated!
	if err := r.syncLocal(ctx, localNs, local, remote); err != nil {
		return ctrl.Result{}, trace.Wrap(err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	name := "sync"
	gvk, err := apiutil.GVKForObject(r.Resource, r.Scheme)
	if kind := strings.ToLower(gvk.Kind); err == nil && kind != "" {
		name += "-" + kind
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		// Translate matching local resources into local resource reconcile requests
		Watches(&source.Kind{Type: r.Resource}, // Remote cluster
			handler.EnqueueRequestsFromMapFunc(r.translateLocal),
		).
		// Translate matching remote resources into local resource reconcile requests
		Watches(source.NewKindWithCache(r.Resource, r.RemoteCache), // Remote cluster
			handler.EnqueueRequestsFromMapFunc(r.translateRemote),
		).
		// Reconcile on namespaces with owner annotations to protect against orphan namespaces
		Watches(&source.Kind{Type: &corev1.Namespace{}},
			handler.EnqueueRequestsFromMapFunc(r.translateNamespace),
		).
		// Sync specified secrets on update
		Watches(&source.Kind{Type: &corev1.Secret{}},
			&handler.EnqueueRequestForOwner{OwnerType: r.Resource, IsController: true},
			builder.WithPredicates(predicate.NewPredicateFuncs(r.filterLocalSecrets)),
		).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.ConcurrentReconciles,
		}).
		Complete(r)
}

// translateLocal translates local object name requests to local object names
func (r *Reconciler) translateLocal(obj client.Object) []reconcile.Request {
	name := obj.GetName()
	namespace := obj.GetNamespace()
	if !strings.HasPrefix(namespace, r.NamespacePrefix) ||
		!strings.HasSuffix(namespace, r.LocalNamespaceSuffix) {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Name:      name,
		Namespace: strings.TrimSuffix(namespace, r.LocalNamespaceSuffix),
	}}}
}

// translateRemote translates remote object name requests to local object names
func (r *Reconciler) translateRemote(obj client.Object) []reconcile.Request {
	name := obj.GetName()
	namespace := obj.GetNamespace()
	if !strings.HasPrefix(namespace, r.NamespacePrefix) ||
		!strings.HasSuffix(name, r.RemoteResourceSuffix) {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Name:      strings.TrimSuffix(name, r.RemoteResourceSuffix),
		Namespace: namespace,
	}}}
}

func (r *Reconciler) translateNamespace(obj client.Object) []reconcile.Request {
	namespace := obj.GetName()
	if !strings.HasPrefix(namespace, r.NamespacePrefix) {
		return nil
	}
	if a := obj.GetAnnotations(); a != nil {
		if name := a[AnnotationNamespaceOwner]; name != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{
				Name:      name,
				Namespace: strings.TrimSuffix(namespace, r.LocalNamespaceSuffix),
			}}}
		}
	}
	return nil
}

// filterLocalSecrets filters local secrets to the requested names.
func (r *Reconciler) filterLocalSecrets(obj client.Object) bool {
	name := obj.GetName()
	namespace := obj.GetNamespace()
	if !strings.HasPrefix(namespace, r.NamespacePrefix) {
		return false
	}
	for _, n := range r.LocalSecretNames {
		if n == name {
			return true
		}
	}
	return false
}

// addFinalizer adds a finalizer to the remote resource.
// This claims the resource. Sync-controller is responsible for cleanup if addFinalizer returns true.
func (r *Reconciler) addFinalizer(ctx context.Context, remote client.Object) (bool, error) {
	log := log.FromContext(ctx)

	// Using real remote could skip validations (e.g., pause annotation)
	// if the remote is reused later.
	remoteC := remote.DeepCopyObject().(client.Object)
	patch := client.MergeFrom(remoteC.DeepCopyObject().(client.Object))
	if ok := controllerutil.AddFinalizer(remoteC, FinalizerRemote); ok {
		if err := r.RemoteClient.Patch(ctx, remoteC, patch); err != nil {
			log.Error(err, "(remote) unable to add finalizer via patch")
			return false, trace.Wrap(err)
		}
		log.Info("(remote) finalizer added")
		return true, nil
	}

	log.V(1).Info("(remote) finalizer already present")
	return false, nil
}

// removeFinalizer removes the finalizer from the remote resource.
// This unclaims the resource.
func (r *Reconciler) removeFinalizer(ctx context.Context, remote client.Object) (bool, error) {
	log := log.FromContext(ctx)

	remoteC := remote.DeepCopyObject().(client.Object)
	patch := client.MergeFrom(remoteC.DeepCopyObject().(client.Object))
	if ok := controllerutil.RemoveFinalizer(remoteC, FinalizerRemote); ok {
		if err := r.RemoteClient.Patch(ctx, remoteC, patch); err != nil {
			log.Error(err, "(remote) unable to remove finalizer via patch")
			return true, trace.Wrap(err)
		}
		log.Info("(remote) removed finalizer")
		return false, nil
	}

	log.V(1).Info("(remote) finalizer already removed")
	return false, nil
}

// syncRemote uploads the local resource's status and secrets to the remote cluster.
func (r *Reconciler) syncRemote(ctx context.Context, local, remote client.Object) error {
	log := log.FromContext(ctx)

	// If local exists (even if being deleted), write its status to remote.
	// If updating status fails, we intentionally fail without updating spec, so that
	// updates are paused while the remote cluster cannot determine current state.

	if creationTime(local).IsZero() {
		log.V(1).Info("(remote) status unavailable locally")
		return nil
	}

	statusRemote := getField(remote, fieldStatus)
	statusLocal := getField(local.DeepCopyObject(), fieldStatus)
	if !equality.Semantic.DeepEqual(statusRemote, statusLocal) {
		// Using real remote would skip validations (e.g., pause annotation)
		// if the remote is reused later.
		remoteC := remote.DeepCopyObject().(client.Object)
		patch := client.MergeFrom(remoteC.DeepCopyObject().(client.Object))
		ok := setField(remoteC, fieldStatus, statusLocal)
		if !ok {
			return errors.New("cannot set status, invalid struct")
		}
		if err := r.RemoteClient.Status().Patch(ctx, remoteC, patch); err != nil {
			log.Error(err, "(remote) unable to patch status")
			return trace.Wrap(err)
		}
		log.Info("(remote) status patched")
	} else {
		log.V(1).Info("(remote) status unchanged")
	}

	return nil
}

// syncLocal downloads the remote resource's spec to the local cluster.
// This method updates both local and remote in-place to their final values.
func (r *Reconciler) syncLocal(ctx context.Context, localNs *corev1.Namespace, local, remote client.Object) error {
	log := log.FromContext(ctx)

	// Create namespace if missing
	if localNs.CreationTimestamp.IsZero() {
		if localNs.Annotations == nil {
			localNs.Annotations = make(map[string]string, 1)
		}
		localNs.Annotations[AnnotationNamespaceOwner] = local.GetName()
		if err := r.Create(ctx, localNs); err != nil {
			log.Error(err, "(local) unable to create namespace")
			return trace.Wrap(err)
		}
	}

	// Create or update the local resource spec, labels, and annotations.
	//
	// This will fail on conflict unnecessarily if the status is updated, but
	// this is necessary to ensure we don't override finalizers or ownerRefs.
	// Update is preferable to Patch, so we guarantee that spec never diverges.
	// This also slows down spec updates in exchange for more granular status updates.
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, local, func() error {
		ok := setField(local, fieldSpec, getField(remote, fieldSpec))
		if !ok {
			return errors.New("cannot set spec, invalid struct")
		}
		anno := remote.GetAnnotations()
		delete(anno, AnnotationRemoteGeneration)
		delete(anno, AnnotationLocalGeneration)
		local.SetAnnotations(anno)
		local.SetLabels(remote.GetLabels())
		return nil
	})
	if err != nil {
		log.Error(err, "(local) unable to update spec and metadata")
		return trace.Wrap(err)
	}
	if op == controllerutil.OperationResultNone {
		log.V(1).Info("(local) spec and updatable metadata " + string(op))
	} else {
		log.Info("(local) spec and updatable metadata " + string(op))
	}

	// Sync generation back to remote cluster as AnnotationLocalGeneration,
	// Also, let remote know that its generation is reflected via AnnotationRemoteGeneration.
	localGen := strconv.FormatInt(local.GetGeneration(), 10)
	remoteGen := strconv.FormatInt(remote.GetGeneration(), 10)
	anno := remote.GetAnnotations()
	if anno[AnnotationLocalGeneration] != localGen ||
		anno[AnnotationRemoteGeneration] != remoteGen {
		patch := client.MergeFrom(remote.DeepCopyObject().(client.Object))
		if anno == nil {
			anno = make(map[string]string, 1)
		}
		anno[AnnotationLocalGeneration] = localGen
		anno[AnnotationRemoteGeneration] = remoteGen
		remote.SetAnnotations(anno)
		if err := r.RemoteClient.Patch(ctx, remote, patch); err != nil {
			log.Error(err, "(remote) unable to update generation annotations")
			return trace.Wrap(err)
		}
		log.Info("(remote) generation annotations updated", logKeyLocalGeneration, localGen, logKeyRemoteGeneration, remoteGen)
	} else {
		log.V(1).Info("(remote) generation annotations unchanged", logKeyLocalGeneration, localGen, logKeyRemoteGeneration, remoteGen)
	}

	return nil
}

// getSecrets retrieves local secrets that meet requirements:
// - Name explicitly requested
// - Controlled by local copy of resource
// getSecrets may return successfully retrieved secrets even if it returns an error.
func (r *Reconciler) getSecrets(ctx context.Context, owner client.Object) ([]*corev1.Secret, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).
		WithValues(logKeyOwner, owner.GetName()))
	log := log.FromContext(ctx)

	var (
		out    []*corev1.Secret
		getErr error
	)
	for _, name := range r.LocalSecretNames {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      name,
			Namespace: owner.GetNamespace(),
		}, secret)
		if apierrors.IsNotFound(err) {
			log.V(1).Info("(local) secret not found", logKeyChild, name)
			continue
		}
		if err != nil {
			log.Error(err, "(local) unable to fetch secret", logKeyChild, name)
			if getErr == nil {
				getErr = trace.Wrap(err)
			}
			continue
		}
		if o := metav1.GetControllerOf(secret); o == nil || o.UID != owner.GetUID() {
			log.V(1).Info("(local) secret not controlled by resource", logKeyChild, name)
			continue
		}
		out = append(out, secret)

	}
	return out, getErr
}

// updateSecrets uploads the provided secrets to the remote cluster
// and ensures that they are controlled by the remote copy of the resource.
func (r *Reconciler) updateSecrets(ctx context.Context, owner client.Object, secrets []*corev1.Secret) error {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).
		WithValues(logKeyOwner, owner.GetName()))
	log := log.FromContext(ctx)

	var updateErr error
	for _, secret := range secrets {
		patch := client.MergeFrom(secret.DeepCopy())
		lsecret := secret.DeepCopy()
		if lsecret.Annotations == nil {
			lsecret.Annotations = make(map[string]string, 1)
		}
		delete(lsecret.Annotations, AnnotationLastUploadedHash)
		hash, err := objHash([]any{
			lsecret.Data, lsecret.Type,
			lsecret.Annotations, lsecret.Labels,
		})
		if err != nil {
			log.Error(err, "(local) failed to hash secret", logKeyChild, lsecret.Name)
			if updateErr == nil {
				updateErr = trace.Wrap(err)
			}
			continue
		}
		lsecret.Annotations[AnnotationLastUploadedHash] = hash

		if secret.Annotations[AnnotationLastUploadedHash] ==
			lsecret.Annotations[AnnotationLastUploadedHash] {
			log.V(1).Info("(local) secret unchanged", logKeyChild, lsecret.Name)
			continue
		}

		rsecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        lsecret.Name + r.RemoteResourceSuffix,
				Namespace:   owner.GetNamespace(),
				Annotations: lsecret.Annotations,
				Labels:      lsecret.Labels,
			},
			Data: lsecret.Data,
			Type: lsecret.Type,
		}
		if err := controllerutil.SetControllerReference(owner, rsecret, r.Scheme); err != nil {
			log.Error(err, "(remote) failed to generate secret", logKeyChild, rsecret.Name)
			if updateErr == nil {
				updateErr = trace.Wrap(err)
			}
			continue
		}
		err = r.RemoteClient.Update(ctx, rsecret)
		if err != nil {
			log.Error(err, "(remote) failed to update secret", logKeyChild, rsecret.Name)
			if updateErr == nil {
				updateErr = trace.Wrap(err)
			}
			continue
		}
		log.Info("(remote) secret updated", logKeyChild, rsecret.Name)

		if err := r.Patch(ctx, lsecret, patch); err != nil {
			log.Error(err, "(local) failed to store secret hash", logKeyChild, lsecret.Name)
			if updateErr == nil {
				updateErr = trace.Wrap(err)
			}
			continue
		}
		log.V(1).Info("(local) secret hash updated", logKeyChild, lsecret.Name)
	}
	return updateErr
}

// objHash returns a hash of the provided object's JSON representation.
func objHash(o any) (string, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(b)
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// getField retries a field from a struct or struct pointer using reflection.
func getField(s any, field string) any {
	r := reflect.ValueOf(s)
	f := reflect.Indirect(r).FieldByName(field)
	return f.Interface()
}

// setField sets a field on a struct pointer.
func setField(s any, field string, v any) (ok bool) {
	e := reflect.ValueOf(s).Elem()
	if e.Kind() != reflect.Struct {
		return false
	}
	f := e.FieldByName(field)
	if !f.IsValid() || !f.CanSet() {
		return false
	}
	f.Set(reflect.ValueOf(v))
	return true
}

// creationTime is needed to call IsZero on client.Object times without extracting variable
func creationTime(obj client.Object) *metav1.Time {
	v := obj.GetCreationTimestamp()
	return &v
}

// setError is used with defer to set earlier error if no error would be returned otherwise
func setError(err error, rerr *error) {
	if *rerr == nil {
		*rerr = err
	}
}
