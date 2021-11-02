// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package seed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	gardencorev1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	"github.com/gardener/gardener/pkg/client/kubernetes/clientmap"
	"github.com/gardener/gardener/pkg/client/kubernetes/clientmap/keys"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/gardenlet/apis/config"
	"github.com/gardener/gardener/pkg/logger"
	"github.com/gardener/gardener/pkg/operation/garden"
	seedpkg "github.com/gardener/gardener/pkg/operation/seed"
	gutil "github.com/gardener/gardener/pkg/utils/gardener"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hyperapi "github.com/openshift/hypershift/api"
	hypershiftassets "github.com/openshift/hypershift/cmd/install/assets"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func (c *Controller) seedAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		logger.Logger.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	c.seedQueue.Add(key)
}

func (c *Controller) seedUpdate(oldObj, newObj interface{}) {
	var (
		oldSeed       = oldObj.(*gardencorev1beta1.Seed)
		newSeed       = newObj.(*gardencorev1beta1.Seed)
		specChanged   = !apiequality.Semantic.DeepEqual(oldSeed.Spec, newSeed.Spec)
		statusChanged = !apiequality.Semantic.DeepEqual(oldSeed.Status, newSeed.Status)
	)

	if !specChanged && statusChanged {
		return
	}
	c.seedAdd(newObj)
}

func (c *Controller) seedDelete(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		logger.Logger.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	c.seedQueue.Add(key)
}

func newReconciler(
	clientMap clientmap.ClientMap,
	recorder record.EventRecorder,
	l logrus.FieldLogger,
	imageVector imagevector.ImageVector,
	componentImageVectors imagevector.ComponentImageVectors,
	identity *gardencorev1beta1.Gardener,
	config *config.GardenletConfiguration,
) reconcile.Reconciler {
	return &reconciler{
		clientMap:             clientMap,
		recorder:              recorder,
		logger:                l,
		imageVector:           imageVector,
		componentImageVectors: componentImageVectors,
		identity:              identity,
		config:                config,
	}
}

type reconciler struct {
	clientMap             clientmap.ClientMap
	recorder              record.EventRecorder
	logger                logrus.FieldLogger
	imageVector           imagevector.ImageVector
	componentImageVectors imagevector.ComponentImageVectors
	identity              *gardencorev1beta1.Gardener
	config                *config.GardenletConfiguration
}

func (r *reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log := r.logger.WithField("seed", request.Name)

	gardenClient, err := r.clientMap.GetClient(ctx, keys.ForGarden())
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get garden client: %w", err)
	}

	seed := &gardencorev1beta1.Seed{}
	gClient := gardenClient.Client()
	if err := gClient.Get(ctx, request.NamespacedName, seed); err != nil {
		if apierrors.IsNotFound(err) {
			log.Debugf("[SEED RECONCILE] skipping because Seed has been deleted")

			if err := r.clientMap.InvalidateClient(keys.ForSeedWithName(request.Name)); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to invalidate seed client: %w", err)
			}

			return reconcile.Result{}, nil
		}
		log.Errorf("[SEED RECONCILE] unable to retrieve object from store: %v", err)
		return reconcile.Result{}, err
	}

	opts := Options{
		Namespace:                  "hypershift",
		HyperShiftImage:            "quay.io/nargaman/hypershift:latest",
		HyperShiftOperatorReplicas: 1,
	}
	var objects []crclient.Object
	objects = append(objects, hyperShiftOperatorManifests(opts)...)
	if err := r.apply(ctx, seed, objects); err != nil {
		log.Errorf("error reconciling seed: *** hypershift error: %v", err)
		return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if err := r.reconcile(ctx, gClient, seed, log); err != nil {
		log.Errorf("error reconciling seed: %v", err)
		return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
	}

	return reconcile.Result{}, nil
}

type Options struct {
	Namespace                  string
	HyperShiftImage            string
	HyperShiftOperatorReplicas int32
	Development                bool
	Render                     bool
	ExcludeEtcdManifests       bool
}

func hyperShiftOperatorManifests(opts Options) []crclient.Object {
	controlPlanePriorityClass := hypershiftassets.HyperShiftControlPlanePriorityClass{}.Build()
	etcdPriorityClass := hypershiftassets.HyperShiftEtcdPriorityClass{}.Build()
	apiCriticalPriorityClass := hypershiftassets.HyperShiftAPICriticalPriorityClass{}.Build()
	operatorNamespace := hypershiftassets.HyperShiftNamespace{
		Name: opts.Namespace,
	}.Build()
	operatorServiceAccount := hypershiftassets.HyperShiftOperatorServiceAccount{
		Namespace: operatorNamespace,
	}.Build()
	operatorClusterRole := hypershiftassets.HyperShiftOperatorClusterRole{}.Build()
	operatorClusterRoleBinding := hypershiftassets.HyperShiftOperatorClusterRoleBinding{
		ClusterRole:    operatorClusterRole,
		ServiceAccount: operatorServiceAccount,
	}.Build()
	operatorRole := hypershiftassets.HyperShiftOperatorRole{
		Namespace: operatorNamespace,
	}.Build()
	operatorRoleBinding := hypershiftassets.HyperShiftOperatorRoleBinding{
		ServiceAccount: operatorServiceAccount,
		Role:           operatorRole,
	}.Build()
	operatorDeployment := hypershiftassets.HyperShiftOperatorDeployment{
		Namespace:      operatorNamespace,
		OperatorImage:  opts.HyperShiftImage,
		ServiceAccount: operatorServiceAccount,
		Replicas:       opts.HyperShiftOperatorReplicas,
	}.Build()
	operatorService := hypershiftassets.HyperShiftOperatorService{
		Namespace: operatorNamespace,
	}.Build()
	prometheusRole := hypershiftassets.HyperShiftPrometheusRole{
		Namespace: operatorNamespace,
	}.Build()
	prometheusRoleBinding := hypershiftassets.HyperShiftOperatorPrometheusRoleBinding{
		Namespace: operatorNamespace,
		Role:      prometheusRole,
	}.Build()
	serviceMonitor := hypershiftassets.HyperShiftServiceMonitor{
		Namespace: operatorNamespace,
	}.Build()

	var objects []crclient.Object

	objects = append(objects, hypershiftassets.CustomResourceDefinitions(func(path string) bool {
		if strings.Contains(path, "etcd") && opts.ExcludeEtcdManifests {
			return false
		}
		return true
	})...)

	objects = append(objects, controlPlanePriorityClass)
	objects = append(objects, apiCriticalPriorityClass)
	objects = append(objects, etcdPriorityClass)
	objects = append(objects, operatorNamespace)
	objects = append(objects, operatorServiceAccount)
	objects = append(objects, operatorClusterRole)
	objects = append(objects, operatorClusterRoleBinding)
	objects = append(objects, operatorRole)
	objects = append(objects, operatorRoleBinding)
	objects = append(objects, operatorDeployment)
	objects = append(objects, operatorService)
	objects = append(objects, prometheusRole)
	objects = append(objects, prometheusRoleBinding)
	objects = append(objects, serviceMonitor)

	return objects
}

func (r *reconciler) apply(ctx context.Context, seed *gardencorev1beta1.Seed, objects []crclient.Object) error {
	for _, object := range objects {
		var objectBytes bytes.Buffer
		err := hyperapi.YamlSerializer.Encode(object, &objectBytes)
		if err != nil {
			return err
		}
		seedClientSet, err := r.clientMap.GetClient(ctx, keys.ForSeed(seed))
		if err != nil {
			return fmt.Errorf("hypershift apply --> failed to get seed client: %w", err)
		}

		seedClient := seedClientSet.Client()
		err = seedClient.Patch(ctx, object, crclient.RawPatch(types.ApplyPatchType, objectBytes.Bytes()), crclient.ForceOwnership, crclient.FieldOwner("hypershift"))
		if err != nil {
			return err
		}
		fmt.Printf("applied %s %s/%s\n", object.GetObjectKind().GroupVersionKind().Kind, object.GetNamespace(), object.GetName())
	}
	return nil
}

func (r *reconciler) reconcile(ctx context.Context, gardenClient client.Client, seed *gardencorev1beta1.Seed, log logrus.FieldLogger) error {
	seedNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: gutil.ComputeGardenNamespace(seed.Name),
		},
	}

	// Check if seed namespace is already available.
	if err := gardenClient.Get(ctx, client.ObjectKeyFromObject(seedNamespace), seedNamespace); err != nil {
		return fmt.Errorf("failed to get seed namespace in garden cluster: %w", err)
	}

	// Initialize capacity and allocatable
	var capacity, allocatable corev1.ResourceList
	if r.config.Resources != nil && len(r.config.Resources.Capacity) > 0 {
		capacity = make(corev1.ResourceList, len(r.config.Resources.Capacity))
		allocatable = make(corev1.ResourceList, len(r.config.Resources.Capacity))
		for resourceName, quantity := range r.config.Resources.Capacity {
			capacity[resourceName] = quantity
			if reservedQuantity, ok := r.config.Resources.Reserved[resourceName]; ok {
				allocatableQuantity := quantity.DeepCopy()
				allocatableQuantity.Sub(reservedQuantity)
				allocatable[resourceName] = allocatableQuantity
			} else {
				allocatable[resourceName] = quantity
			}
		}
	}

	// Initialize conditions based on the current status.
	conditionSeedBootstrapped := gardencorev1beta1helper.GetOrInitCondition(seed.Status.Conditions, gardencorev1beta1.SeedBootstrapped)

	seedObj, err := seedpkg.
		NewBuilder().
		WithSeedObject(seed).
		Build(ctx)
	if err != nil {
		message := fmt.Sprintf("Failed to create a Seed object (%s).", err.Error())
		conditionSeedBootstrapped = gardencorev1beta1helper.UpdatedCondition(conditionSeedBootstrapped, gardencorev1beta1.ConditionUnknown, gardencorev1beta1.ConditionCheckError, message)
		log.Error(message)
		_ = r.patchSeedStatus(ctx, gardenClient, log, seed, "<unknown>", capacity, allocatable, conditionSeedBootstrapped)
		return err
	}

	seedClientSet, err := r.clientMap.GetClient(ctx, keys.ForSeed(seed))
	if err != nil {
		return fmt.Errorf("failed to get seed client: %w", err)
	}

	// The deletionTimestamp labels a Seed as intended to get deleted. Before deletion,
	// it has to be ensured that no Shoots are depending on the Seed anymore.
	// When this happens the controller will remove the finalizers from the Seed so that it can be garbage collected.
	if seed.DeletionTimestamp != nil {
		if !sets.NewString(seed.Finalizers...).Has(gardencorev1beta1.GardenerName) {
			return nil
		}

		if seed.Spec.Backup != nil {
			if err := deleteBackupBucketInGarden(ctx, gardenClient, seed); err != nil {
				log.Error(err.Error())
				return err
			}
		}

		associatedShoots, err := controllerutils.DetermineShootsAssociatedTo(ctx, gardenClient, seed)
		if err != nil {
			log.Error(err.Error())
			return err
		}

		associatedBackupBuckets, err := controllerutils.DetermineBackupBucketAssociations(ctx, gardenClient, seed.Name)
		if err != nil {
			log.Error(err.Error())
			return err
		}

		if len(associatedShoots) == 0 && len(associatedBackupBuckets) == 0 {
			log.Info("No Shoots, ControllerInstallations or BackupBuckets are referencing the Seed. Deletion accepted.")

			if err := seedpkg.RunDeleteSeedFlow(ctx, gardenClient, seedClientSet, seedObj, log); err != nil {
				message := fmt.Sprintf("Failed to delete Seed Cluster (%s).", err.Error())
				conditionSeedBootstrapped = gardencorev1beta1helper.UpdatedCondition(conditionSeedBootstrapped, gardencorev1beta1.ConditionFalse, "DebootstrapFailed", message)
				log.Error(message)
				_ = r.patchSeedStatus(ctx, gardenClient, log, seed, "<unknown>", capacity, allocatable, conditionSeedBootstrapped)
				return err
			}

			if seed.Spec.SecretRef != nil {
				// Remove finalizer from referenced secret
				secret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      seed.Spec.SecretRef.Name,
						Namespace: seed.Spec.SecretRef.Namespace,
					},
				}
				err = gardenClient.Get(ctx, client.ObjectKeyFromObject(secret), secret)
				if err == nil {
					if err2 := controllerutils.PatchRemoveFinalizers(ctx, gardenClient, secret, gardencorev1beta1.ExternalGardenerName); err2 != nil {
						return fmt.Errorf("failed to remove finalizer from Seed secret '%s/%s': %w", secret.Namespace, secret.Name, err2)
					}
				} else if !apierrors.IsNotFound(err) {
					return fmt.Errorf("failed to get Seed secret '%s/%s': %w", secret.Namespace, secret.Name, err)
				}
			}

			// Remove finalizer from Seed
			if err := controllerutils.PatchRemoveFinalizers(ctx, gardenClient, seed, gardencorev1beta1.GardenerName); err != nil {
				log.Error(err.Error())
				return err
			}

			if err := r.clientMap.InvalidateClient(keys.ForSeed(seed)); err != nil {
				return fmt.Errorf("failed to invalidate seed client: %w", err)
			}

			return nil
		}

		parentLogMessage := "Can't delete Seed, because the following objects are still referencing it:"
		if len(associatedShoots) != 0 {
			message := fmt.Sprintf("%s Shoots=%v", parentLogMessage, associatedShoots)
			log.Info(message)
			r.recorder.Event(seed, corev1.EventTypeNormal, v1beta1constants.EventResourceReferenced, message)
		}
		if len(associatedBackupBuckets) != 0 {
			message := fmt.Sprintf("%s BackupBuckets=%v", parentLogMessage, associatedBackupBuckets)
			log.Info(message)
			r.recorder.Event(seed, corev1.EventTypeNormal, v1beta1constants.EventResourceReferenced, message)
		}

		return errors.New("seed still has references")
	}

	log.Infof("[SEED RECONCILE]")

	// TODO(timebertt): replace with strategic merge, so we can run without optimistic locking
	// (make use of $deleteFromPrimitiveList directive)
	if !controllerutil.ContainsFinalizer(seed, gardencorev1beta1.GardenerName) {
		if err := controllerutils.PatchAddFinalizers(ctx, gardenClient, seed, gardencorev1beta1.GardenerName); err != nil {
			err = fmt.Errorf("could not add finalizer to Seed: %s", err.Error())
			log.Error(err)
			return err
		}
	}

	// Add the Gardener finalizer to the referenced Seed secret to protect it from deletion as long as the Seed resource
	// does exist.
	if seed.Spec.SecretRef != nil {
		secret, err := kutil.GetSecretByReference(ctx, gardenClient, seed.Spec.SecretRef)
		if err != nil {
			log.Error(err.Error())
			return err
		}
		if err := controllerutils.PatchAddFinalizers(ctx, gardenClient, secret, gardencorev1beta1.ExternalGardenerName); err != nil {
			log.Error(err.Error())
			return err
		}
	}

	// Check whether the Kubernetes version of the Seed cluster fulfills the minimal requirements.
	seedKubernetesVersion, err := seedObj.CheckMinimumK8SVersion(seedClientSet.Version())
	if err != nil {
		conditionSeedBootstrapped = gardencorev1beta1helper.UpdatedCondition(conditionSeedBootstrapped, gardencorev1beta1.ConditionFalse, "K8SVersionTooOld", err.Error())
		_ = r.patchSeedStatus(ctx, gardenClient, log, seed, seedKubernetesVersion, capacity, allocatable, conditionSeedBootstrapped)
		log.Error(err.Error())
		return err
	}

	gardenSecrets, err := garden.ReadGardenSecrets(ctx, gardenClient, gutil.ComputeGardenNamespace(seed.Name), log)
	if err != nil {
		conditionSeedBootstrapped = gardencorev1beta1helper.UpdatedCondition(conditionSeedBootstrapped, gardencorev1beta1.ConditionFalse, "GardenSecretsError", err.Error())
		_ = r.patchSeedStatus(ctx, gardenClient, log, seed, seedKubernetesVersion, capacity, allocatable, conditionSeedBootstrapped)
		log.Errorf("Reading Garden secrets failed: %+v", err)
		return err
	}

	// Bootstrap the Seed cluster.
	if err := seedpkg.RunReconcileSeedFlow(ctx, gardenClient, seedClientSet, seedObj, gardenSecrets, r.imageVector, r.componentImageVectors, r.config.DeepCopy(), log); err != nil {
		conditionSeedBootstrapped = gardencorev1beta1helper.UpdatedCondition(conditionSeedBootstrapped, gardencorev1beta1.ConditionFalse, "BootstrappingFailed", err.Error())
		_ = r.patchSeedStatus(ctx, gardenClient, log, seed, seedKubernetesVersion, capacity, allocatable, conditionSeedBootstrapped)
		log.Errorf("Seed bootstrapping failed: %+v", err)
		return err
	}

	conditionSeedBootstrapped = gardencorev1beta1helper.UpdatedCondition(conditionSeedBootstrapped, gardencorev1beta1.ConditionTrue, "BootstrappingSucceeded", "Seed cluster has been bootstrapped successfully.")
	_ = r.patchSeedStatus(ctx, gardenClient, log, seed, seedKubernetesVersion, capacity, allocatable, conditionSeedBootstrapped)

	if seed.Spec.Backup != nil {
		// This should be post updating the seed is available. Since, scheduler will then mostly use
		// same seed for deploying the backupBucket extension.
		if err := deployBackupBucketInGarden(ctx, gardenClient, seed); err != nil {
			log.Error(err.Error())
			return err
		}
	}

	return nil
}

func (r *reconciler) patchSeedStatus(
	ctx context.Context,
	c client.Client,
	log logrus.FieldLogger,
	seed *gardencorev1beta1.Seed,
	seedVersion string,
	capacity, allocatable corev1.ResourceList,
	updateConditions ...gardencorev1beta1.Condition,
) error {
	patch := client.StrategicMergeFrom(seed.DeepCopy())

	seed.Status.Conditions = gardencorev1beta1helper.MergeConditions(seed.Status.Conditions, updateConditions...)
	seed.Status.ObservedGeneration = seed.Generation
	seed.Status.Gardener = r.identity
	seed.Status.KubernetesVersion = &seedVersion
	seed.Status.Capacity = capacity
	seed.Status.Allocatable = allocatable

	if err := c.Status().Patch(ctx, seed, patch); err != nil {
		log.Errorf("Could not update the Seed status: %+v", err)
		return err
	}

	return nil
}

func deployBackupBucketInGarden(ctx context.Context, k8sGardenClient client.Client, seed *gardencorev1beta1.Seed) error {
	// By default, we assume the seed.Spec.Backup.Provider matches the seed.Spec.Provider.Type as per the validation logic.
	// However, if the backup region is specified we take it.
	region := seed.Spec.Provider.Region
	if seed.Spec.Backup.Region != nil {
		region = *seed.Spec.Backup.Region
	}

	backupBucket := &gardencorev1beta1.BackupBucket{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(seed.UID),
		},
	}

	ownerRef := metav1.NewControllerRef(seed, gardencorev1beta1.SchemeGroupVersion.WithKind("Seed"))

	_, err := controllerutils.CreateOrGetAndStrategicMergePatch(ctx, k8sGardenClient, backupBucket, func() error {
		backupBucket.OwnerReferences = []metav1.OwnerReference{*ownerRef}
		backupBucket.Spec = gardencorev1beta1.BackupBucketSpec{
			Provider: gardencorev1beta1.BackupBucketProvider{
				Type:   seed.Spec.Backup.Provider,
				Region: region,
			},
			ProviderConfig: seed.Spec.Backup.ProviderConfig,
			SecretRef: corev1.SecretReference{
				Name:      seed.Spec.Backup.SecretRef.Name,
				Namespace: seed.Spec.Backup.SecretRef.Namespace,
			},
			SeedName: &seed.Name, // In future this will be moved to gardener-scheduler.
		}
		return nil
	})
	return err
}

func deleteBackupBucketInGarden(ctx context.Context, k8sGardenClient client.Client, seed *gardencorev1beta1.Seed) error {
	backupBucket := &gardencorev1beta1.BackupBucket{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(seed.UID),
		},
	}

	return client.IgnoreNotFound(k8sGardenClient.Delete(ctx, backupBucket))
}
