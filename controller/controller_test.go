package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bombsimon/logrusr/v4"
	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestSync(t *testing.T) {
	testScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(testScheme); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	logger := logrus.StandardLogger()
	logger.SetLevel(10)
	logf.SetLogger(logrusr.New(logger))
	ctx = logr.NewContext(ctx, logrusr.New(logger))

	testEnv := &envtest.Environment{
		Scheme: testScheme,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Fatal(err)
		}
	})

	testClient, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatal(err)
	}

	r := &Reconciler{
		Client:                 testClient,
		RemoteClient:           testClient,
		Scheme:                 testScheme,
		Resource:               &policyv1.PodDisruptionBudget{},
		RemoteResourceSuffix:   "-remote-rs",
		LocalNamespaceSuffix:   "-local-ns",
		LocalSecretNames:       []string{"secret1", "secret2"},
		LocalPropagationPolicy: client.PropagationPolicy(metav1.DeletePropagationBackground),
	}

	// Only one of localIn or remoteIn is required.
	// If remoteIn is specified it will be used for the request,
	// otherwise localIn will be used.
	// Output resources must not be nil. Use empty value to represent delete.
	// Names and namespaces will be generated automatically, so leave them black.
	for _, tc := range []struct {
		desc string
		err  error

		remoteIn, remoteOut client.Object
		localIn, localOut   client.Object

		localSecretsIn   []*corev1.Secret
		remoteSecretsOut []*corev1.Secret

		skipCreateNamespace bool
		skipNamespaceOwner  bool
	}{
		{
			desc: "Sync metadata, spec, status, and secrets",
			remoteIn: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"remote": "label"},
					Annotations: map[string]string{"remote": "anno"},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 1,
				},
			},
			remoteOut: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"remote": "label"},
					Annotations: map[string]string{
						"remote":                   "anno",
						AnnotationLocalGeneration:  "2",
						AnnotationRemoteGeneration: "1",
					},
					Finalizers: []string{FinalizerRemote},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			localIn: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"local": "label"},
					Annotations: map[string]string{"local": "anno"},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(1),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			localOut: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"remote": "label"},
					Annotations: map[string]string{"remote": "anno"},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			localSecretsIn: []*corev1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "secret1"},
					StringData: map[string]string{"data": "data1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "secret2"},
					StringData: map[string]string{"data": "data2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "secret3"},
					StringData: map[string]string{"data": "data3"},
				},
			},
			remoteSecretsOut: []*corev1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "secret1-remote-rs",
						Annotations: map[string]string{
							"cloud.teleport.dev/last-uploaded-hash": "4bc3db4c8600b8a668677befadd94408e27cdcac611dcb4d4f56e0d2dcb3e1c5",
						},
					},
					Type: corev1.SecretTypeOpaque,
					Data: map[string][]byte{"data": []byte("data1")},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "secret2-remote-rs",
						Annotations: map[string]string{
							"cloud.teleport.dev/last-uploaded-hash": "1c67e1d274f4a7bedb1d1ee0f23f3c962e170d20e1f480c419a9c68f5efb3a0f",
						},
					},
					Type: corev1.SecretTypeOpaque,
					Data: map[string][]byte{"data": []byte("data2")},
				},
			},
		},
		{
			desc: "Paused remotely",
			remoteIn: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{AnnotationPaused: "true"},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(1),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 1,
				},
			},
			remoteOut: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{AnnotationPaused: "true"},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(1),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 1,
				},
			},
			localIn: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			localOut: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			localSecretsIn: []*corev1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "secret1"},
					StringData: map[string]string{"data": "data1"},
				},
			},
			remoteSecretsOut: nil,
		},
		{
			desc: "Paused locally",
			remoteIn: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(1),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 1,
				},
			},
			remoteOut: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(1),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 1,
				},
			},
			localIn: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{AnnotationPaused: "true"},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			localOut: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{AnnotationPaused: "true"},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			localSecretsIn: []*corev1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "secret1"},
					StringData: map[string]string{"data": "data1"},
				},
			},
			remoteSecretsOut: nil,
		},
		{
			desc: "Local is missing and created, unowned secrets not uploaded",
			remoteIn: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 1,
				},
			},
			remoteOut: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{FinalizerRemote},
					Annotations: map[string]string{
						AnnotationLocalGeneration:  "1",
						AnnotationRemoteGeneration: "1",
					},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 0,
				},
			},
			localOut: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},

				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 0,
				},
			},
			localSecretsIn: []*corev1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "secret1"},
					StringData: map[string]string{"data": "data1"},
				},
			},
			remoteSecretsOut: nil,
		},
		{
			desc: "Local and local namespace is missing and created",
			remoteIn: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 1,
				},
			},
			remoteOut: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{FinalizerRemote},
					Annotations: map[string]string{
						AnnotationLocalGeneration:  "1",
						AnnotationRemoteGeneration: "1",
					},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 0,
				},
			},
			localOut: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 0,
				},
			},
			remoteSecretsOut:    nil,
			skipCreateNamespace: true,
		},
		{
			desc:      "Remote deleted causing owned local namespace to be deleted",
			remoteOut: &policyv1.PodDisruptionBudget{},
			localIn: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			localOut: &policyv1.PodDisruptionBudget{},
			localSecretsIn: []*corev1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "secret1"},
					StringData: map[string]string{"data": "data1"},
				},
			},
			remoteSecretsOut: nil,
		},
		{
			desc:      "Remote deleted causing local resource to be deleted when namespace unowned",
			remoteOut: &policyv1.PodDisruptionBudget{},
			localIn: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			localOut: &policyv1.PodDisruptionBudget{},
			localSecretsIn: []*corev1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "secret1"},
					StringData: map[string]string{"data": "data1"},
				},
			},
			remoteSecretsOut:   nil,
			skipNamespaceOwner: true,
		},
		{
			desc: "Remote being deleted with finalizer causing local resource to be deleted",
			remoteIn: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{FinalizerRemote},
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			remoteOut: &policyv1.PodDisruptionBudget{},
			localIn: &policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: toIntstr(2),
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					CurrentHealthy: 2,
				},
			},
			localOut: &policyv1.PodDisruptionBudget{},
			localSecretsIn: []*corev1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "secret1"},
					StringData: map[string]string{"data": "data1"},
				},
			},
			remoteSecretsOut:   nil,
			skipNamespaceOwner: true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			// Create namespaces with input values
			remoteNs, localNs := setupTestCase(t, ctx, &setupConfig{
				client:               testClient,
				scheme:               testScheme,
				localIn:              tc.localIn,
				remoteIn:             tc.remoteIn,
				localSecretsIn:       tc.localSecretsIn,
				skipCreateNamespace:  tc.skipCreateNamespace,
				skipNamespaceOwner:   tc.skipNamespaceOwner,
				remoteResourceSuffix: r.RemoteResourceSuffix,
				localNamespaceSuffix: r.LocalNamespaceSuffix,
			})

			// Add generated name/namespace to expected values
			tc.localOut.SetName("test")
			tc.localOut.SetNamespace(remoteNs.Name + r.LocalNamespaceSuffix)
			tc.remoteOut.SetName("test" + r.RemoteResourceSuffix)
			tc.remoteOut.SetNamespace(strings.TrimSuffix(localNs.Name, r.LocalNamespaceSuffix))

			// Generate request
			var req reconcile.Request
			if tc.remoteIn != nil {
				req = r.translateRemote(ctx, tc.remoteIn)[0]
			} else if tc.localIn != nil {
				req = r.translateLocal(ctx, tc.localIn)[0]
			}

			// Reconcile for finalizer
			res, err := r.Reconcile(ctx, req)
			if diff := cmp.Diff(tc.err, err); diff != "" {
				t.Errorf("Error mismatch (-want +got):\n%s", diff)
			}
			// No requeue loops
			if diff := cmp.Diff(reconcile.Result{}, res); diff != "" {
				t.Errorf("Result mismatch (-want +got):\n%s", diff)
			}

			// Reconcile for deletes
			res, err = r.Reconcile(ctx, req)
			if diff := cmp.Diff(tc.err, err); diff != "" {
				t.Errorf("Error mismatch (-want +got):\n%s", diff)
			}
			// No requeue loops
			if diff := cmp.Diff(reconcile.Result{}, res); diff != "" {
				t.Errorf("Result mismatch (-want +got):\n%s", diff)
			}

			// Reconcile for sync
			res, err = r.Reconcile(ctx, req)
			if diff := cmp.Diff(tc.err, err); diff != "" {
				t.Errorf("Error mismatch (-want +got):\n%s", diff)
			}
			// No requeue loops
			if diff := cmp.Diff(reconcile.Result{}, res); diff != "" {
				t.Errorf("Result mismatch (-want +got):\n%s", diff)
			}

			// Compare remote resource and secrets
			remoteOut := tc.remoteOut.DeepCopyObject().(client.Object)
			if err := testClient.Get(ctx, client.ObjectKeyFromObject(remoteOut), remoteOut); err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.remoteOut, filterMetadata(remoteOut)); diff != "" {
				t.Errorf("Remote mismatch (-want +got):\n%s", diff)
			}
			secrets := &corev1.SecretList{}
			if err := testClient.List(ctx, secrets); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.remoteSecretsOut, filterSecrets(secrets.Items, remoteOut)); diff != "" {
				t.Errorf("Secret mismatch (-want +got):\n%s", diff)
			}

			// Compare local resource
			localOut := tc.localOut.DeepCopyObject().(client.Object)
			if err := testClient.Get(ctx, client.ObjectKeyFromObject(localOut), localOut); err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
			// Workaround for envtest bug: if namespace is being deleted, pretend localOut is deleted
			if err := testClient.Get(ctx, client.ObjectKeyFromObject(localNs), localNs); err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
			if !localNs.CreationTimestamp.IsZero() &&
				tc.skipCreateNamespace && !tc.skipNamespaceOwner {
				if diff := cmp.Diff(localOut.GetName(), localNs.Annotations[AnnotationNamespaceOwner]); diff != "" {
					t.Errorf("Local namespace annotation mismatch (-want +got):\n%s", diff)
				}
			}
			if !localNs.DeletionTimestamp.IsZero() {
				name, namespace := localOut.GetName(), localOut.GetNamespace()
				localOut = r.Resource.DeepCopyObject().(client.Object)
				localOut.SetName(name)
				localOut.SetNamespace(namespace)
			}
			if diff := cmp.Diff(tc.localOut, filterMetadata(localOut)); diff != "" {
				t.Errorf("Local mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

type setupConfig struct {
	client               client.Client
	scheme               *runtime.Scheme
	localIn, remoteIn    client.Object
	localSecretsIn       []*corev1.Secret
	skipCreateNamespace  bool
	skipNamespaceOwner   bool
	remoteResourceSuffix string
	localNamespaceSuffix string
}

func setupTestCase(t *testing.T, ctx context.Context, cfg *setupConfig) (remoteNs, localNs *corev1.Namespace) {
	// Create namespaces
	remoteNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "test",
	}}
	if err := cfg.client.Create(ctx, remoteNs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cfg.client.Delete(ctx, remoteNs); err != nil {
			t.Fatal(err)
		}
	})

	localNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: remoteNs.Name + cfg.localNamespaceSuffix,
	}}
	if !cfg.skipNamespaceOwner && cfg.localIn != nil {
		localNs.Annotations = map[string]string{
			AnnotationNamespaceOwner: cfg.localIn.GetName(),
		}
	}
	if !cfg.skipCreateNamespace {
		if err := cfg.client.Create(ctx, localNs); err != nil {
			t.Fatal(err)
		}
	} else {
		if cfg.localIn != nil || len(cfg.localSecretsIn) > 0 {
			t.Fatal("skipCreateNamespace incompatible with localIn and localSecretsIn")
		}
	}
	t.Cleanup(func() {
		if err := cfg.client.Delete(ctx, localNs); err != nil {
			t.Fatal(err)
		}
	})

	// Create resources and secrets (if specified)
	if cfg.localIn != nil {
		cfg.localIn.SetName("test")
		cfg.localIn.SetNamespace(localNs.Name)
		status := getField(cfg.localIn.DeepCopyObject(), fieldStatus)
		if err := cfg.client.Create(ctx, cfg.localIn); err != nil {
			t.Fatal(err)
		}
		setField(cfg.localIn, fieldStatus, status)
		if err := cfg.client.Status().Update(ctx, cfg.localIn); err != nil {
			t.Fatal(err)
		}
	}
	if cfg.remoteIn != nil {
		cfg.remoteIn.SetName("test" + cfg.remoteResourceSuffix)
		cfg.remoteIn.SetNamespace(remoteNs.Name)

		status := getField(cfg.remoteIn.DeepCopyObject(), fieldStatus)
		if err := cfg.client.Create(ctx, cfg.remoteIn); err != nil {
			t.Fatal(err)
		}
		setField(cfg.remoteIn, fieldStatus, status)
		if err := cfg.client.Status().Update(ctx, cfg.remoteIn); err != nil {
			t.Fatal(err)
		}
		if len(cfg.remoteIn.GetFinalizers()) > 0 {
			if err := cfg.client.Delete(ctx, cfg.remoteIn); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, secret := range cfg.localSecretsIn {
		secret.SetNamespace(localNs.Name)
		if cfg.localIn != nil {
			err := controllerutil.SetControllerReference(cfg.localIn, secret, cfg.scheme)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := cfg.client.Create(ctx, secret); err != nil {
			t.Fatal(err)
		}

		remoteSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secret.Name + cfg.remoteResourceSuffix,
				Namespace: remoteNs.Name,
			},
		}
		if err := cfg.client.Create(ctx, remoteSecret); err != nil {
			t.Fatal(err)
		}
	}

	return remoteNs, localNs
}

func filterMetadata(obj client.Object) client.Object {
	obj = obj.DeepCopyObject().(client.Object)
	obj.SetResourceVersion("")
	obj.SetGeneration(0)
	obj.SetCreationTimestamp(metav1.NewTime(time.Time{}))
	obj.SetOwnerReferences(nil)
	obj.SetManagedFields(nil)
	obj.SetUID("")
	return obj
}

func filterSecrets(secrets []corev1.Secret, owner client.Object) []*corev1.Secret {
	var out []*corev1.Secret
	for _, secret := range secrets {
		c := secret.DeepCopyObject().(client.Object)
		if o := metav1.GetControllerOf(c); o == nil || o.UID != owner.GetUID() {
			continue
		}
		c.SetNamespace("")
		out = append(out, filterMetadata(c).(*corev1.Secret))
	}
	return out
}

func toIntstr(v int) *intstr.IntOrString {
	i := intstr.FromInt(v)
	return &i
}
