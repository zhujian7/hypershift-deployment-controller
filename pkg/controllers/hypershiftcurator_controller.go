/*
Copyright 2022.

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

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	hypdeployment "github.com/jnpacker/hypershift-deployment-controller/api/v1alpha1"
	hyp "github.com/openshift/hypershift/api/v1alpha1"
	"github.com/openshift/hypershift/cmd/infra/aws"
	"github.com/openshift/hypershift/cmd/util"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
)

// HypershiftDeploymentReconciler reconciles a HypershiftDeployment object
type HypershiftDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	ctx    context.Context
	Log    logr.Logger
}

const (
	destroyFinalizer       = "hypershift.openshift.io/finalizer"
	HostedClusterFinalizer = "hypershift.openshift.io/used-by-hostedcluster"
	oidcStorageProvider    = "oidc-storage-provider-s3-config"
	oidcSPNamespace        = "kube-public"
	AutoInfraLabelName     = "hypershift.openshift.io/auto-created-for-infra"
	InfraLabelName         = "hypershift.openshift.io/infra-id"
)

//+kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=hypershiftdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=hypershiftdeployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=hypershiftdeployments/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the HypershiftDeployment object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile
func (r *HypershiftDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log = log.FromContext(ctx)
	log := r.Log
	r.ctx = ctx

	var hyd hypdeployment.HypershiftDeployment
	if err := r.Get(ctx, req.NamespacedName, &hyd); err != nil {
		log.V(2).Info("Resource deleted")
		return ctrl.Result{}, nil
	}

	var providerSecret corev1.Secret
	var err error

	if hyd.Spec.Infrastructure.Configure {
		err = r.Client.Get(r.ctx, types.NamespacedName{Namespace: hyd.Namespace, Name: hyd.Spec.Infrastructure.CloudProvider.Name}, &providerSecret)
		if err != nil {
			log.Error(err, "Could not retrieve the provider secret")
			return ctrl.Result{RequeueAfter: 30 * time.Second, Requeue: true}, nil
		}
	}

	if hyd.Spec.InfraID == "" {
		hyd.Spec.InfraID = fmt.Sprintf("%s-%s", hyd.GetName(), utilrand.String(5))
		log.Info("Using INFRA-ID: " + hyd.Spec.InfraID)

		controllerutil.AddFinalizer(&hyd, destroyFinalizer)
		//metav1.SetMetaDataLabel(&hyd.ObjectMeta, InfraLabelName, hyd.SpecInfraID)

		if err := r.updateHypershiftDeploymentResource(&hyd); err != nil || hyd.Spec.InfraID == "" {
			return ctrl.Result{}, fmt.Errorf("failed to update infra-id: %w", err)
		}

		//Update the status.conditions. This only works the first time, so if you fix an issue, it will still be set to PlatformXXXMisConfigured
		setStatusCondition(&hyd, hypdeployment.PlatformConfigured, metav1.ConditionFalse, "Configuring platform with infra-id: "+hyd.Spec.InfraID, hypdeployment.PlatformBeingConfigured)
		r.updateStatusConditionsOnChange(&hyd, hypdeployment.PlatformIAMConfigured, metav1.ConditionFalse, "Configuring platform IAM with infra-id: "+hyd.Spec.InfraID, hypdeployment.PlatformIAMBeingConfigured)
	}

	// Destroying Platform infrastructure used by the HypershiftDeployment scheduled for deletion
	if hyd.DeletionTimestamp != nil {
		return r.destroyHypershift(&hyd, &providerSecret)
	}

	if !hyd.Spec.Infrastructure.Configure {
		log.Info("Nothing to configure")
		return ctrl.Result{}, nil
	}

	if hyd.Spec.Infrastructure.Platform == nil {
		return ctrl.Result{}, r.updateMissingInfrastructureParameterCondition(&hyd, "Missing value HypershiftDeployment.Spec.Infrastructure.Platform")
	}

	var infraOut *aws.CreateInfraOutput
	var iamOut *aws.CreateIAMOutput

	if hyd.Spec.Infrastructure.Platform.AWS != nil {
		if hyd.Spec.Infrastructure.Platform.AWS.Region == "" {
			return ctrl.Result{}, r.updateMissingInfrastructureParameterCondition(&hyd, "Missing value HypershiftDeployment.Spec.Infrastructure.Platform.AWS.Region")
		}

		// Skip reconcile based on condition
		if !meta.IsStatusConditionTrue(hyd.Status.Conditions, string(hypdeployment.PlatformConfigured)) {
			// Creating infrastructure used by the HypershiftDeployment, HostedClusters & NodePools
			o := aws.CreateInfraOptions{
				AWSKey:       string(providerSecret.Data["aws_access_key_id"]),
				AWSSecretKey: string(providerSecret.Data["aws_secret_access_key"]),
				Region:       hyd.Spec.Infrastructure.Platform.AWS.Region,
				InfraID:      hyd.Spec.InfraID,
				Name:         hyd.GetName(),
				BaseDomain:   string(providerSecret.Data["baseDomain"]),
			}

			infraOut, err = o.CreateInfra(r.ctx)
			if err != nil {
				log.Error(err, "Could not create infrastructure")

				return ctrl.Result{RequeueAfter: 1 * time.Minute, Requeue: true},
					r.updateStatusConditionsOnChange(
						&hyd, hypdeployment.PlatformConfigured,
						metav1.ConditionFalse,
						err.Error(),
						hypdeployment.PlatformMisConfiguredReason)
			}

			// This creates the required HostedClusterSpec and NodePoolSpec(s), from scratch or if supplied
			ScafoldHostedClusterSpec(&hyd, infraOut)
			ScafoldNodePoolSpec(&hyd, infraOut)

			if err := r.updateHypershiftDeploymentResource(&hyd); err != nil {
				r.updateStatusConditionsOnChange(&hyd, hypdeployment.PlatformConfigured, metav1.ConditionFalse, err.Error(), hypdeployment.PlatformMisConfiguredReason)
				return ctrl.Result{}, err
			}

			r.updateStatusConditionsOnChange(&hyd, hypdeployment.PlatformConfigured, metav1.ConditionTrue, "", hypdeployment.PlatformConfiguredAsExpected)
			log.Info("Infrastructure configured")

			// This allows more interleaving of reconciles
			return ctrl.Result{}, nil
		}

		if !meta.IsStatusConditionTrue(hyd.Status.Conditions, string(hypdeployment.PlatformIAMConfigured)) {

			if err := r.Get(ctx, req.NamespacedName, &hyd); err != nil {
				return ctrl.Result{}, nil
			}

			oidcSPName, oidcSPRegion, iamErr := oidcDiscoveryURL(r, hyd.Spec.InfraID)
			if iamErr == nil {
				iamOpt := aws.CreateIAMOptions{
					Region:                          hyd.Spec.Infrastructure.Platform.AWS.Region,
					AWSKey:                          string(providerSecret.Data["aws_access_key_id"]),
					AWSSecretKey:                    string(providerSecret.Data["aws_secret_access_key"]),
					InfraID:                         hyd.Spec.InfraID,
					IssuerURL:                       "", //This is generated on the fly by CreateIAMOutput
					AdditionalTags:                  []string{},
					OIDCStorageProviderS3BucketName: oidcSPName,
					OIDCStorageProviderS3Region:     oidcSPRegion,
				}

				iamOut, iamErr = iamOpt.CreateIAM(r.ctx, r.Client)
				if iamErr == nil {
					if iamErr = createOIDCSecrets(r, &hyd, iamOut); iamErr == nil {
						if iamErr = r.createPullSecret(&hyd, providerSecret); iamErr == nil {
							hyd.Spec.HostedClusterSpec.IssuerURL = iamOut.IssuerURL
							hyd.Spec.HostedClusterSpec.Platform.AWS.Roles = iamOut.Roles
							if err := r.updateHypershiftDeploymentResource(&hyd); err != nil {
								return ctrl.Result{}, r.updateStatusConditionsOnChange(&hyd, hypdeployment.PlatformIAMConfigured, metav1.ConditionFalse, err.Error(), hypdeployment.PlatformIAMMisConfiguredReason)
							}
							r.updateStatusConditionsOnChange(&hyd, hypdeployment.PlatformIAMConfigured, metav1.ConditionTrue, "", hypdeployment.PlatformIAMConfiguredAsExpected)
							log.Info("IAM and Secrets configured")

						}
					}
				}
			}
			if iamErr != nil {
				r.updateStatusConditionsOnChange(&hyd, hypdeployment.PlatformIAMConfigured, metav1.ConditionFalse, iamErr.Error(), hypdeployment.PlatformIAMMisConfiguredReason)
				return ctrl.Result{RequeueAfter: 1 * time.Minute, Requeue: true}, iamErr
			}
			// This allows more interleaving of reconciles
			return ctrl.Result{}, nil
		}
	}

	// Work on the HostedCluster resource
	var hc hyp.HostedCluster
	err = r.Get(ctx, types.NamespacedName{Namespace: hyd.Namespace, Name: hyd.Name}, &hc)

	if meta.IsStatusConditionTrue(hyd.Status.Conditions, string(hypdeployment.PlatformIAMConfigured)) &&
		meta.IsStatusConditionTrue(hyd.Status.Conditions, string(hypdeployment.PlatformConfigured)) {
		if errors.IsNotFound(err) {
			hostedCluster := ScafoldHostedCluster(&hyd, hyd.Spec.HostedClusterSpec)
			if err := r.Create(ctx, hostedCluster); err != nil {
				if errors.IsAlreadyExists(err) {
					log.Error(err, "Failed to create HostedCluster resource")
					return ctrl.Result{}, err
				}
				log.Info("HostedCluster created " + hc.Name)

			}
			log.Info("HostedCluster resource created: " + hostedCluster.Name)
		} else {
			if !reflect.DeepEqual(hc.Spec.Autoscaling, hyd.Spec.HostedClusterSpec.Autoscaling) ||
				!reflect.DeepEqual(hc.Spec.Release, hyd.Spec.HostedClusterSpec.Release) ||
				!reflect.DeepEqual(hc.Spec.ControllerAvailabilityPolicy, hyd.Spec.HostedClusterSpec.ControllerAvailabilityPolicy) {
				hc.Spec = *hyd.Spec.HostedClusterSpec
				if err := r.Update(ctx, &hc); err != nil {
					log.Error(err, "Failed to update HostedCluster resource")
					return ctrl.Result{}, err
				}
				log.Info("HostedCluster resource updated: " + hc.Name)
			}
		}
	}

	// Work on the NodePool resources
	if meta.IsStatusConditionTrue(hyd.Status.Conditions, string(hypdeployment.PlatformIAMConfigured)) &&
		meta.IsStatusConditionTrue(hyd.Status.Conditions, string(hypdeployment.PlatformConfigured)) {

		// We loop through what exists, so that we can delete pools if appropriate
		var nodePools hyp.NodePoolList
		if err := r.List(ctx, &nodePools, client.MatchingLabels{AutoInfraLabelName: hyd.Spec.InfraID}); err != nil {
			return ctrl.Result{}, err
		}

		// Create and Update HypershiftDeployment.Spec.NodePools
		for _, np := range hyd.Spec.NodePools {
			noMatch := true
			for _, foundNodePool := range nodePools.Items {
				if np.Name == foundNodePool.Name {
					if !reflect.DeepEqual(foundNodePool.Spec, np.Spec) {
						foundNodePool.Spec = np.Spec
						if err := r.Update(ctx, &foundNodePool); err != nil {
							log.Error(err, "Failed to update NodePool resource")
							return ctrl.Result{RequeueAfter: 10 * time.Second, Requeue: true}, nil
						}
						log.Info("NodePool resource updated: " + np.Name)
					}
					noMatch = false
					break
				}
			}
			if noMatch {
				nodePool := ScafoldNodePool(hyd.Namespace, hyd.Spec.InfraID, np)
				if err := r.Create(ctx, nodePool); err != nil {
					log.Error(err, "Failed to create NodePool resource")
					return ctrl.Result{RequeueAfter: 10 * time.Second, Requeue: true}, nil
				}
				log.Info("NodePool resource created: " + np.Name)
			}
		}

		// Delete a NodePool if it no longer is present in the HypershiftDeployment.Spec.NodePools
		for _, nodePool := range nodePools.Items {
			noMatch := true
			for _, np := range hyd.Spec.NodePools {
				if nodePool.Name == np.Name {
					noMatch = false
				}
			}
			if noMatch {
				if nodePool.DeletionTimestamp == nil {
					if err := r.Delete(ctx, &nodePool); err != nil {
						log.Error(err, "Failed to delete NodePool resource")
						return ctrl.Result{RequeueAfter: 10 * time.Second, Requeue: true}, nil
					}
					log.Info("NodePool resource deleted: " + nodePool.Name)
				}
			}

		}
	}
	return ctrl.Result{}, nil
}

func oidcDiscoveryURL(r *HypershiftDeploymentReconciler, infraID string) (string, string, error) {

	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: oidcStorageProvider, Namespace: oidcSPNamespace}, cm); err != nil {
		return "", "", err
	}
	return cm.Data["name"], cm.Data["region"], nil
}

func (r *HypershiftDeploymentReconciler) createPullSecret(hyd *hypdeployment.HypershiftDeployment, providerSecret corev1.Secret) error {

	buildPullSecret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: hyd.Namespace,
			Name:      hyd.Name + "-pull-secret",
			Labels: map[string]string{
				AutoInfraLabelName: hyd.Spec.InfraID,
			},
		},
		Data: map[string][]byte{
			".dockerconfigjson": providerSecret.Data["pullSecret"],
		},
	}
	if err := r.Create(r.ctx, buildPullSecret); apierrors.IsAlreadyExists(err) {
		if err := r.Update(r.ctx, buildPullSecret); err != nil {
			return err
		}
	}
	return nil
}

func createOIDCSecrets(r *HypershiftDeploymentReconciler, hyd *hypdeployment.HypershiftDeployment, iamInfo *aws.CreateIAMOutput) error {

	buildAWSCreds := func(name, arn string) *corev1.Secret {
		return &corev1.Secret{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Secret",
				APIVersion: corev1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: hyd.Namespace,
				Name:      name,
				Labels: map[string]string{
					AutoInfraLabelName: hyd.Spec.InfraID,
				},
			},
			Data: map[string][]byte{
				"credentials": []byte(fmt.Sprintf(`[default]
	role_arn = %s
	web_identity_token_file = /var/run/secrets/openshift/serviceaccount/token
	`, arn)),
			},
		}
	}

	secretResource := buildAWSCreds(hyd.Name+"-cpo-creds", iamInfo.ControlPlaneOperatorRoleARN)
	if err := r.Create(r.ctx, secretResource); apierrors.IsAlreadyExists(err) {
		if err := r.Update(r.ctx, secretResource); err != nil {
			return err
		}
	}

	secretResource = buildAWSCreds(hyd.Name+"-cloud-ctrl-creds", iamInfo.KubeCloudControllerRoleARN)
	if err := r.Create(r.ctx, secretResource); apierrors.IsAlreadyExists(err) {
		if err := r.Update(r.ctx, secretResource); err != nil {
			return err
		}
	}

	secretResource = buildAWSCreds(hyd.Name+"-node-mgmt-creds", iamInfo.NodePoolManagementRoleARN)
	if err := r.Create(r.ctx, secretResource); apierrors.IsAlreadyExists(err) {
		if err := r.Update(r.ctx, secretResource); err != nil {
			return err
		}

	}
	return nil
}

func destroyOIDCSecrets(r *HypershiftDeploymentReconciler, hyd *hypdeployment.HypershiftDeployment) error {
	//clean up CLI generated secrets
	return r.DeleteAllOf(r.ctx, &corev1.Secret{}, client.InNamespace(hyd.GetNamespace()), client.MatchingLabels{util.AutoInfraLabelName: hyd.Spec.InfraID})

}

func setStatusCondition(hyd *hypdeployment.HypershiftDeployment, conditionType hypdeployment.ConditionType, status metav1.ConditionStatus, message string, reason string) metav1.Condition {
	if hyd.Status.Conditions == nil {
		hyd.Status.Conditions = []metav1.Condition{}
	}
	condition := metav1.Condition{
		Type:               string(conditionType),
		ObservedGeneration: hyd.Generation,
		Status:             status,
		Message:            message,
		Reason:             reason,
	}
	meta.SetStatusCondition(&hyd.Status.Conditions, condition)
	return condition
}

func (r *HypershiftDeploymentReconciler) updateMissingInfrastructureParameterCondition(hyd *hypdeployment.HypershiftDeployment, message string) error {
	setStatusCondition(hyd, hypdeployment.PlatformIAMConfigured, metav1.ConditionFalse, "Infrastructure missing information", hypdeployment.PlatformIAMMisConfiguredReason)
	return r.updateStatusConditionsOnChange(hyd, hypdeployment.PlatformConfigured, metav1.ConditionFalse, message, hypdeployment.PlatformMisConfiguredReason)
}

func (r *HypershiftDeploymentReconciler) updateStatusConditionsOnChange(hyd *hypdeployment.HypershiftDeployment, conditionType hypdeployment.ConditionType, conditionStatus metav1.ConditionStatus, message string, reason string) error {

	var err error = nil
	sc := meta.FindStatusCondition(hyd.Status.Conditions, string(conditionType))
	if sc == nil || sc.ObservedGeneration != hyd.Generation || sc.Status != conditionStatus || sc.Reason != reason || sc.Message != message {
		setStatusCondition(hyd, conditionType, conditionStatus, message, reason)
		r.Log.Info("namespace: " + hyd.GetNamespace())
		err = r.Client.Status().Update(r.ctx, hyd)
		if err != nil {
			if apierrors.IsConflict(err) {
				r.Log.Error(err, "Conflict encountered when updating HypershiftDeployment.Status")
			} else {
				r.Log.Error(err, "Failed to update HypershiftDeployment.Status")
			}
		}
	}
	return err
}

func (r *HypershiftDeploymentReconciler) updateHypershiftDeploymentResource(hyd *hypdeployment.HypershiftDeployment) error {
	err := r.Client.Update(r.ctx, hyd)
	if err != nil {
		if apierrors.IsConflict(err) {
			r.Log.Error(err, "Conflict encountered when updating HypershiftDeployment")
		} else {
			r.Log.Error(err, "Failed to update HypershiftDeployment resource")
		}
	}
	return err
}

func (r *HypershiftDeploymentReconciler) destroyHypershift(hyd *hypdeployment.HypershiftDeployment, providerSecret *corev1.Secret) (ctrl.Result, error) {
	log := r.Log
	ctx := r.ctx

	// Delete nodepools first
	for _, np := range hyd.Spec.NodePools {
		var nodePool hyp.NodePool
		if err := r.Get(ctx, types.NamespacedName{Namespace: hyd.Namespace, Name: np.Name}, &nodePool); !errors.IsNotFound(err) {
			if nodePool.DeletionTimestamp == nil {
				r.Log.Info("Deleting NodePool " + np.Name)
				if err := r.Delete(ctx, &nodePool); err != nil {
					log.Error(err, "Failed to delete NodePool resource")
					return ctrl.Result{RequeueAfter: 10 * time.Second, Requeue: true}, nil
				}
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second, Requeue: true}, nil
		}
	}

	// Delete the HostedCluster
	var hc hyp.HostedCluster
	if err := r.Get(ctx, types.NamespacedName{Namespace: hyd.Namespace, Name: hyd.Name}, &hc); !errors.IsNotFound(err) {
		if hc.DeletionTimestamp == nil {
			r.Log.Info("Deleting HostedCluster " + hyd.Name)
			if err := r.Delete(ctx, &hc); err != nil {
				log.Error(err, "Failed to delete HostedCluster resource")
				return ctrl.Result{RequeueAfter: 10 * time.Second, Requeue: true}, nil
			}
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second, Requeue: true}, nil
	}

	// Infrastructure is the last step
	dOpts := aws.DestroyInfraOptions{
		AWSCredentialsFile: "",
		AWSKey:             string(providerSecret.Data["aws_access_key_id"]),
		AWSSecretKey:       string(providerSecret.Data["aws_secret_access_key"]),
		Region:             hyd.Spec.Infrastructure.Platform.AWS.Region,
		BaseDomain:         string(providerSecret.Data["baseDomain"]),
		InfraID:            hyd.Spec.InfraID,
		Name:               hyd.GetName(),
	}

	setStatusCondition(hyd, hypdeployment.PlatformConfigured, metav1.ConditionFalse, "Destroying HypershiftDeployment with infra-id: "+hyd.Spec.InfraID, hypdeployment.PlatfromDestroy)
	r.updateStatusConditionsOnChange(hyd, hypdeployment.PlatformIAMConfigured, metav1.ConditionFalse, "Removing HypershiftDeployment IAM with infra-id: "+hyd.Spec.InfraID, hypdeployment.PlatformIAMRemove)

	if err := dOpts.DestroyInfra(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to destroy HypershiftDeployment: %w", err)
	}

	iamOpt := aws.DestroyIAMOptions{
		Region:       hyd.Spec.Infrastructure.Platform.AWS.Region,
		AWSKey:       dOpts.AWSKey,
		AWSSecretKey: dOpts.AWSSecretKey,
		InfraID:      dOpts.InfraID,
	}

	if err := iamOpt.DestroyIAM(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete IAM HypershiftDeployment: %w", err)
	}

	if err := destroyOIDCSecrets(r, hyd); err != nil {
		log.Error(err, "Encountered an issue while deleting secrets")
	}

	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: hyd.Namespace, Name: hyd.Name}, hyd); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update HypershiftDeployment values when removing finalizer: %w", err)
	}

	controllerutil.RemoveFinalizer(hyd, destroyFinalizer)

	if err := r.Client.Update(ctx, hyd); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer, update status: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HypershiftDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hypdeployment.HypershiftDeployment{}).
		Complete(r)
}
