/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package externalsecret

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	esv1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	"github.com/external-secrets/external-secrets/pkg/provider"

	// Loading registered providers.
	_ "github.com/external-secrets/external-secrets/pkg/provider/register"
	"github.com/external-secrets/external-secrets/pkg/provider/schema"
	"github.com/external-secrets/external-secrets/pkg/utils"
)

const (
	requeueAfter = time.Second * 30

	errGetES                 = "could not get ExternalSecret"
	errReconcileES           = "could not reconcile ExternalSecret"
	errPatchStatus           = "unable to patch status"
	errGetSecretStore        = "could not get SecretStore %q, %w"
	errGetClusterSecretStore = "could not get ClusterSecretStore %q, %w"
	errStoreRef              = "could not get store reference"
	errStoreProvider         = "could not get store provider"
	errStoreClient           = "could not get provider client"
	errGetExistingSecret     = "could not get existing secret: %w"
	errCloseStoreClient      = "could not close provider client"
	errSetCtrlReference      = "could not set ExternalSecret controller reference: %w"
	errFetchTplFrom          = "error fetching templateFrom data: %w"
	errGetSecretData         = "could not get secret data from provider: %w"
	errApplyTemplate         = "could not apply template: %w"
	errExecTpl               = "could not execute template: %w"
	errPolicyMergeNotFound   = "the desired secret %s was not found. With creationPolicy=Merge the secret won't be created"
	errPolicyMergeGetSecret  = "unable to get secret %s: %w"
	errPolicyMergeMutate     = "unable to mutate secret %s: %w"
	errPolicyMergePatch      = "unable to patch secret %s: %w"
	errGetSecretKey          = "key %q from ExternalSecret %q: %w"
	errTplCMMissingKey       = "error in configmap %s: missing key %s"
	errTplSecMissingKey      = "error in secret %s: missing key %s"
)

// Reconciler reconciles a ExternalSecret object.
type Reconciler struct {
	client.Client
	Log             logr.Logger
	Scheme          *runtime.Scheme
	ControllerClass string
	RequeueInterval time.Duration
}

// Reconcile implements the main reconciliation loop
// for watched objects (ExternalSecret, ClusterSecretStore and SecretStore),
// and updates/creates a Kubernetes secret based on them.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ExternalSecret", req.NamespacedName)

	syncCallsMetricLabels := prometheus.Labels{"name": req.Name, "namespace": req.Namespace}

	var externalSecret esv1alpha1.ExternalSecret

	err := r.Get(ctx, req.NamespacedName, &externalSecret)
	if apierrors.IsNotFound(err) {
		syncCallsTotal.With(syncCallsMetricLabels).Inc()
		conditionSynced := NewExternalSecretCondition(esv1alpha1.ExternalSecretDeleted, v1.ConditionFalse, esv1alpha1.ConditionReasonSecretDeleted, "Secret was deleted")
		SetExternalSecretCondition(&esv1alpha1.ExternalSecret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      req.Name,
				Namespace: req.Namespace,
			},
		}, *conditionSynced)
		return ctrl.Result{}, nil
	} else if err != nil {
		log.Error(err, errGetES)
		syncCallsError.With(syncCallsMetricLabels).Inc()
		return ctrl.Result{}, nil
	}

	// patch status when done processing
	p := client.MergeFrom(externalSecret.DeepCopy())
	defer func() {
		err = r.Status().Patch(ctx, &externalSecret, p)
		if err != nil {
			log.Error(err, errPatchStatus)
		}
	}()

	store, err := r.getStore(ctx, &externalSecret)
	if err != nil {
		log.Error(err, errStoreRef)
		conditionSynced := NewExternalSecretCondition(esv1alpha1.ExternalSecretReady, v1.ConditionFalse, esv1alpha1.ConditionReasonSecretSyncedError, err.Error())
		SetExternalSecretCondition(&externalSecret, *conditionSynced)
		syncCallsError.With(syncCallsMetricLabels).Inc()
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	log = log.WithValues("SecretStore", store.GetNamespacedName())

	// check if store should be handled by this controller instance
	if !shouldProcessStore(store, r.ControllerClass) {
		log.Info("skipping unmanaged store")
		return ctrl.Result{}, nil
	}

	storeProvider, err := schema.GetProvider(store)
	if err != nil {
		log.Error(err, errStoreProvider)
		syncCallsError.With(syncCallsMetricLabels).Inc()
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	secretClient, err := storeProvider.NewClient(ctx, store, r.Client, req.Namespace)
	if err != nil {
		log.Error(err, errStoreClient)
		conditionSynced := NewExternalSecretCondition(esv1alpha1.ExternalSecretReady, v1.ConditionFalse, esv1alpha1.ConditionReasonSecretSyncedError, err.Error())
		SetExternalSecretCondition(&externalSecret, *conditionSynced)
		syncCallsError.With(syncCallsMetricLabels).Inc()
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	defer func() {
		err = secretClient.Close(ctx)
		if err != nil {
			log.Error(err, errCloseStoreClient)
		}
	}()

	refreshInt := r.RequeueInterval
	if externalSecret.Spec.RefreshInterval != nil {
		refreshInt = externalSecret.Spec.RefreshInterval.Duration
	}

	// Target Secret Name should default to the ExternalSecret name if not explicitly specified
	secretName := externalSecret.Spec.Target.Name
	if secretName == "" {
		secretName = externalSecret.ObjectMeta.Name
	}

	// fetch external secret, we need to ensure that it exists, and it's hashmap corresponds
	var existingSecret v1.Secret
	err = r.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: externalSecret.Namespace,
	}, &existingSecret)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, errGetExistingSecret)
	}

	// refresh should be skipped if
	// 1. resource generation hasn't changed
	// 2. refresh interval is 0
	// 3. if we're still within refresh-interval
	if !shouldRefresh(externalSecret) && isSecretValid(existingSecret) {
		log.V(1).Info("skipping refresh", "rv", getResourceVersion(externalSecret))
		return ctrl.Result{RequeueAfter: refreshInt}, nil
	}

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: externalSecret.Namespace,
		},
		Data: make(map[string][]byte),
	}

	mutationFunc := func() error {
		if externalSecret.Spec.Target.CreationPolicy == esv1alpha1.Owner {
			err = controllerutil.SetControllerReference(&externalSecret, &secret.ObjectMeta, r.Scheme)
			if err != nil {
				return fmt.Errorf(errSetCtrlReference, err)
			}
		}

		dataMap, err := r.getProviderSecretData(ctx, secretClient, &externalSecret)
		if err != nil {
			return fmt.Errorf(errGetSecretData, err)
		}

		err = r.applyTemplate(ctx, &externalSecret, secret, dataMap)
		if err != nil {
			return fmt.Errorf(errApplyTemplate, err)
		}

		return nil
	}

	// nolint
	switch externalSecret.Spec.Target.CreationPolicy {
	case esv1alpha1.Merge:
		err = patchSecret(ctx, r.Client, r.Scheme, secret, mutationFunc)
	case esv1alpha1.None:
		log.V(1).Info("secret creation skipped due to creationPolicy=None")
		err = nil
	default:
		_, err = ctrl.CreateOrUpdate(ctx, r.Client, secret, mutationFunc)
	}

	if err != nil {
		log.Error(err, errReconcileES)
		conditionSynced := NewExternalSecretCondition(esv1alpha1.ExternalSecretReady, v1.ConditionFalse, esv1alpha1.ConditionReasonSecretSyncedError, err.Error())
		SetExternalSecretCondition(&externalSecret, *conditionSynced)
		syncCallsError.With(syncCallsMetricLabels).Inc()
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	conditionSynced := NewExternalSecretCondition(esv1alpha1.ExternalSecretReady, v1.ConditionTrue, esv1alpha1.ConditionReasonSecretSynced, "Secret was synced")
	SetExternalSecretCondition(&externalSecret, *conditionSynced)
	externalSecret.Status.RefreshTime = metav1.NewTime(time.Now())
	externalSecret.Status.SyncedResourceVersion = getResourceVersion(externalSecret)
	syncCallsTotal.With(syncCallsMetricLabels).Inc()
	log.V(1).Info("reconciled secret")

	return ctrl.Result{
		RequeueAfter: refreshInt,
	}, nil
}

func patchSecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, secret *v1.Secret, mutationFunc func() error) error {
	err := c.Get(ctx, client.ObjectKeyFromObject(secret), secret.DeepCopy())
	if apierrors.IsNotFound(err) {
		return fmt.Errorf(errPolicyMergeNotFound, secret.Name)
	}
	if err != nil {
		return fmt.Errorf(errPolicyMergeGetSecret, secret.Name, err)
	}
	err = mutationFunc()
	if err != nil {
		return fmt.Errorf(errPolicyMergeMutate, secret.Name, err)
	}
	// GVK is missing in the Secret, see:
	// https://github.com/kubernetes-sigs/controller-runtime/issues/526
	// https://github.com/kubernetes-sigs/controller-runtime/issues/1517
	// https://github.com/kubernetes/kubernetes/issues/80609
	// we need to manually set it before doing a Patch() as it depends on the GVK
	gvks, unversioned, err := scheme.ObjectKinds(secret)
	if err != nil {
		return err
	}
	if !unversioned && len(gvks) == 1 {
		secret.SetGroupVersionKind(gvks[0])
	}
	// we might get into a conflict here if we are not the manager of that particular field
	// we do not resolve the conflict and return an error instead
	// see: https://kubernetes.io/docs/reference/using-api/server-side-apply/#conflicts
	err = c.Patch(ctx, secret, client.Apply, client.FieldOwner("external-secrets"))
	if err != nil {
		return fmt.Errorf(errPolicyMergePatch, secret.Name, err)
	}
	return nil
}

// shouldProcessStore returns true if the store should be processed.
func shouldProcessStore(store esv1alpha1.GenericStore, class string) bool {
	if store.GetSpec().Controller == "" || store.GetSpec().Controller == class {
		return true
	}
	return false
}

func getResourceVersion(es esv1alpha1.ExternalSecret) string {
	return fmt.Sprintf("%d-%s", es.ObjectMeta.GetGeneration(), hashMeta(es.ObjectMeta))
}

func hashMeta(m metav1.ObjectMeta) string {
	type meta struct {
		annotations map[string]string
		labels      map[string]string
	}
	return utils.ObjectHash(meta{
		annotations: m.Annotations,
		labels:      m.Labels,
	})
}

func shouldRefresh(es esv1alpha1.ExternalSecret) bool {
	// refresh if resource version changed
	if es.Status.SyncedResourceVersion != getResourceVersion(es) {
		return true
	}

	// skip refresh if refresh interval is 0
	if es.Spec.RefreshInterval.Duration == 0 && es.Status.SyncedResourceVersion != "" {
		return false
	}
	if es.Status.RefreshTime.IsZero() {
		return true
	}
	return !es.Status.RefreshTime.Add(es.Spec.RefreshInterval.Duration).After(time.Now())
}

// isSecretValid checks if the secret exists, and it's data is consistent with the calculated hash.
func isSecretValid(existingSecret v1.Secret) bool {
	// if target secret doesn't exist, or annotations as not set, we need to refresh
	if existingSecret.UID == "" || existingSecret.Annotations == nil {
		return false
	}

	// if the calculated hash is different from the calculation, then it's invalid
	if existingSecret.Annotations[esv1alpha1.AnnotationDataHash] != utils.ObjectHash(existingSecret.Data) {
		return false
	}
	return true
}

// getStore returns the store with the provided ExternalSecret.
func (r *Reconciler) getStore(ctx context.Context, externalSecret *esv1alpha1.ExternalSecret) (esv1alpha1.GenericStore, error) {
	ref := types.NamespacedName{
		Name: externalSecret.Spec.SecretStoreRef.Name,
	}

	if externalSecret.Spec.SecretStoreRef.Kind == esv1alpha1.ClusterSecretStoreKind {
		var store esv1alpha1.ClusterSecretStore
		err := r.Get(ctx, ref, &store)
		if err != nil {
			return nil, fmt.Errorf(errGetClusterSecretStore, ref.Name, err)
		}

		return &store, nil
	}

	ref.Namespace = externalSecret.Namespace

	var store esv1alpha1.SecretStore
	err := r.Get(ctx, ref, &store)
	if err != nil {
		return nil, fmt.Errorf(errGetSecretStore, ref.Name, err)
	}
	return &store, nil
}

// getProviderSecretData returns the provider's secret data with the provided ExternalSecret.
func (r *Reconciler) getProviderSecretData(ctx context.Context, providerClient provider.SecretsClient, externalSecret *esv1alpha1.ExternalSecret) (map[string][]byte, error) {
	providerData := make(map[string][]byte)

	for _, remoteRef := range externalSecret.Spec.DataFrom {
		secretMap, err := providerClient.GetSecretMap(ctx, remoteRef)
		if err != nil {
			return nil, fmt.Errorf(errGetSecretKey, remoteRef.Key, externalSecret.Name, err)
		}

		providerData = utils.MergeByteMap(providerData, secretMap)
	}

	for _, secretRef := range externalSecret.Spec.Data {
		secretData, err := providerClient.GetSecret(ctx, secretRef.RemoteRef)
		if err != nil {
			return nil, fmt.Errorf(errGetSecretKey, secretRef.RemoteRef.Key, externalSecret.Name, err)
		}

		providerData[secretRef.SecretKey] = secretData
	}

	return providerData, nil
}

// SetupWithManager returns a new controller builder that will be started by the provided Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&esv1alpha1.ExternalSecret{}).
		Owns(&v1.Secret{}).
		Complete(r)
}