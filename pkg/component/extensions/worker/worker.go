// Copyright 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	v1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/component"
	"github.com/gardener/gardener/pkg/component/extensions/operatingsystemconfig"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/extensions"
	gardenerutils "github.com/gardener/gardener/pkg/utils/gardener"
	"github.com/gardener/gardener/pkg/utils/gardener/shootstate"
)

const (
	// DefaultInterval is the default interval for retry operations.
	DefaultInterval = 5 * time.Second
	// DefaultSevereThreshold is the default threshold until an error reported by another component is treated as
	// 'severe'.
	DefaultSevereThreshold = 30 * time.Second
	// DefaultTimeout is the default timeout and defines how long Gardener should wait for a successful reconciliation
	// of a Worker resource.
	DefaultTimeout = 10 * time.Minute
)

// TimeNow returns the current time. Exposed for testing.
var TimeNow = time.Now

// Interface is an interface for managing Workers.
type Interface interface {
	component.DeployMigrateWaiter
	SetSSHPublicKey([]byte)
	SetInfrastructureProviderStatus(*runtime.RawExtension)
	SetWorkerNameToOperatingSystemConfigsMap(map[string]*operatingsystemconfig.OperatingSystemConfigs)
	MachineDeployments() []extensionsv1alpha1.MachineDeployment
	WaitUntilWorkerStatusMachineDeploymentsUpdated(ctx context.Context) error
}

// Values contains the values used to create a Worker resources.
type Values struct {
	// Namespace is the Shoot namespace in the seed.
	Namespace string
	// Name is the name of the Worker resource.
	Name string
	// Type is the type of the Worker provider.
	Type string
	// Region is the region of the shoot.
	Region string
	// Workers is the list of worker pools.
	Workers []gardencorev1beta1.Worker
	// KubernetesVersion is the Kubernetes version of the cluster for which the worker nodes shall be created.
	KubernetesVersion *semver.Version
	// MachineTypes is the list of machine types present in the CloudProfile referenced by the shoot
	MachineTypes []gardencorev1beta1.MachineType
	// SSHPublicKey is the public SSH key that shall be installed on the worker nodes.
	SSHPublicKey []byte
	// InfrastructureProviderStatus is the provider status of the Infrastructure resource which might be relevant for
	// the Worker reconciliation.
	InfrastructureProviderStatus *runtime.RawExtension
	// WorkerNameToOperatingSystemConfigsMap contains the operating system configurations for the worker pools.
	WorkerNameToOperatingSystemConfigsMap map[string]*operatingsystemconfig.OperatingSystemConfigs
	// NodeLocalDNSEnabled indicates whether node local dns is enabled or not.
	NodeLocalDNSEnabled bool
}

// New creates a new instance of Interface.
func New(
	log logr.Logger,
	client client.Client,
	values *Values,
	waitInterval time.Duration,
	waitSevereThreshold time.Duration,
	waitTimeout time.Duration,
) Interface {
	return &worker{
		log:                 log,
		client:              client,
		values:              values,
		waitInterval:        waitInterval,
		waitSevereThreshold: waitSevereThreshold,
		waitTimeout:         waitTimeout,

		worker: &extensionsv1alpha1.Worker{
			ObjectMeta: metav1.ObjectMeta{
				Name:      values.Name,
				Namespace: values.Namespace,
			},
		},
	}
}

type worker struct {
	values              *Values
	log                 logr.Logger
	client              client.Client
	waitInterval        time.Duration
	waitSevereThreshold time.Duration
	waitTimeout         time.Duration

	worker                           *extensionsv1alpha1.Worker
	machineDeployments               []extensionsv1alpha1.MachineDeployment
	machineDeploymentsLastUpdateTime *metav1.Time
}

// Deploy uses the seed client to create or update the Worker resource.
func (w *worker) Deploy(ctx context.Context) error {
	_, err := w.deploy(ctx, v1beta1constants.GardenerOperationReconcile)
	return err
}

func (w *worker) deploy(ctx context.Context, operation string) (extensionsv1alpha1.Object, error) {
	var pools []extensionsv1alpha1.WorkerPool

	obj := &extensionsv1alpha1.Worker{}
	if err := w.client.Get(ctx, client.ObjectKey{Name: w.worker.Name, Namespace: w.worker.Namespace}, obj); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}

	for _, workerPool := range w.values.Workers {
		var volume *extensionsv1alpha1.Volume
		if workerPool.Volume != nil {
			volume = &extensionsv1alpha1.Volume{
				Name:      workerPool.Volume.Name,
				Type:      workerPool.Volume.Type,
				Size:      workerPool.Volume.VolumeSize,
				Encrypted: workerPool.Volume.Encrypted,
			}
		}

		var dataVolumes []extensionsv1alpha1.DataVolume
		if len(workerPool.DataVolumes) > 0 {
			for _, dataVolume := range workerPool.DataVolumes {
				dataVolumes = append(dataVolumes, extensionsv1alpha1.DataVolume{
					Name:      dataVolume.Name,
					Type:      dataVolume.Type,
					Size:      dataVolume.VolumeSize,
					Encrypted: dataVolume.Encrypted,
				})
			}
		}

		var pConfig *runtime.RawExtension
		if workerPool.ProviderConfig != nil {
			pConfig = &runtime.RawExtension{
				Raw: workerPool.ProviderConfig.Raw,
			}
		}

		var userData []byte
		if val, ok := w.values.WorkerNameToOperatingSystemConfigsMap[workerPool.Name]; ok {
			userData = []byte(val.Downloader.Content)
		}

		workerPoolKubernetesVersion := w.values.KubernetesVersion.String()
		if workerPool.Kubernetes != nil && workerPool.Kubernetes.Version != nil {
			workerPoolKubernetesVersion = *workerPool.Kubernetes.Version
		}

		nodeTemplate, machineType := w.findNodeTemplateAndMachineTypeByPoolName(obj, workerPool.Name)

		if nodeTemplate == nil || machineType != workerPool.Machine.Type {
			// initializing nodeTemplate by fetching details from cloudprofile, if present there
			if machineDetails := v1beta1helper.FindMachineTypeByName(w.values.MachineTypes, workerPool.Machine.Type); machineDetails != nil {
				nodeTemplate = &extensionsv1alpha1.NodeTemplate{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    machineDetails.CPU,
						"gpu":                 machineDetails.GPU,
						corev1.ResourceMemory: machineDetails.Memory,
					},
				}
			} else {
				nodeTemplate = nil
			}
		}

		pools = append(pools, extensionsv1alpha1.WorkerPool{
			Name:           workerPool.Name,
			Minimum:        workerPool.Minimum,
			Maximum:        workerPool.Maximum,
			MaxSurge:       *workerPool.MaxSurge,
			MaxUnavailable: *workerPool.MaxUnavailable,
			Annotations:    workerPool.Annotations,
			Labels:         gardenerutils.NodeLabelsForWorkerPool(workerPool, w.values.NodeLocalDNSEnabled),
			Taints:         workerPool.Taints,
			MachineType:    workerPool.Machine.Type,
			MachineImage: extensionsv1alpha1.MachineImage{
				Name:    workerPool.Machine.Image.Name,
				Version: *workerPool.Machine.Image.Version,
			},
			NodeTemplate:                     nodeTemplate,
			ProviderConfig:                   pConfig,
			UserData:                         userData,
			Volume:                           volume,
			DataVolumes:                      dataVolumes,
			KubeletDataVolumeName:            workerPool.KubeletDataVolumeName,
			KubernetesVersion:                &workerPoolKubernetesVersion,
			Zones:                            workerPool.Zones,
			MachineControllerManagerSettings: workerPool.MachineControllerManagerSettings,
			Architecture:                     workerPool.Machine.Architecture,
		})
	}

	// We operate on arrays (pools) with merge patch without optimistic locking here, meaning this will replace
	// the arrays as a whole.
	// However, this is not a problem, as no other client should write to these arrays as the Worker spec is supposed
	// to be owned by gardenlet exclusively.
	_, err := controllerutils.GetAndCreateOrMergePatch(ctx, w.client, w.worker, func() error {
		metav1.SetMetaDataAnnotation(&w.worker.ObjectMeta, v1beta1constants.GardenerOperation, operation)
		metav1.SetMetaDataAnnotation(&w.worker.ObjectMeta, v1beta1constants.GardenerTimestamp, TimeNow().UTC().Format(time.RFC3339Nano))

		w.worker.Spec = extensionsv1alpha1.WorkerSpec{
			DefaultSpec: extensionsv1alpha1.DefaultSpec{
				Type: w.values.Type,
			},
			Region: w.values.Region,
			SecretRef: corev1.SecretReference{
				Name:      v1beta1constants.SecretNameCloudProvider,
				Namespace: w.worker.Namespace,
			},
			SSHPublicKey:                 w.values.SSHPublicKey,
			InfrastructureProviderStatus: w.values.InfrastructureProviderStatus,
			Pools:                        pools,
		}

		return nil
	})

	// populate the MachineDeploymentsLastUpdate time as it will be used later to confirm if the machineDeployments slice in the worker
	// status got updated with the latest ones.
	w.machineDeploymentsLastUpdateTime = obj.Status.MachineDeploymentsLastUpdateTime

	return w.worker, err
}

// Restore uses the seed client and the ShootState to create the Worker resources and restore their state.
func (w *worker) Restore(ctx context.Context, shootState *gardencorev1beta1.ShootState) error {
	// gardenlet persists the machine state in the ShootState's `.spec.gardener[]` list with `type=machine-state`.
	// In the future, the provider extension's Worker controller is expected to read the machine state directly from the
	// ShootState resource in the garden cluster, and use it to recreate the actual machine.saploud.io/v1alpha1 objects.
	// However: For backwards-compatibility, we have to make the machine state also available in the Worker object's
	// `.status.state` field since older versions (< v1.81) of the generic `Worker` actuator's `Restore` function expect
	// to find the state here, see https://github.com/gardener/gardener/blob/422e2bbedd23351383154bb733838a416f39f2b6/extensions/pkg/controller/worker/genericactuator/actuator_restore.go#L121C1-L141.
	// TODO(rfranzke): Drop this code after Gardener v1.86 has been released.
	var (
		shootStateCopy = shootState.DeepCopy()
		gardenerData   = v1beta1helper.GardenerResourceDataList(shootStateCopy.Spec.Gardener)
	)

	if machineState := gardenerData.Get(v1beta1constants.DataTypeMachineState); machineState != nil && machineState.Type == v1beta1constants.DataTypeMachineState {
		machineStateDecompressed, err := shootstate.DecompressMachineState(machineState.Data.Raw)
		if err != nil {
			return err
		}
		extensionsData := v1beta1helper.ExtensionResourceStateList(shootStateCopy.Spec.Extensions)
		extensionsData.Upsert(&gardencorev1beta1.ExtensionResourceState{
			Kind:  extensionsv1alpha1.WorkerResource,
			Name:  &w.worker.Name,
			State: &runtime.RawExtension{Raw: machineStateDecompressed},
		})
		shootStateCopy.Spec.Extensions = extensionsData
	}

	return extensions.RestoreExtensionWithDeployFunction(
		ctx,
		w.client,
		shootStateCopy,
		extensionsv1alpha1.WorkerResource,
		w.deploy,
	)
}

// Migrate migrates the Worker resource.
func (w *worker) Migrate(ctx context.Context) error {
	return extensions.MigrateExtensionObject(
		ctx,
		w.client,
		w.worker,
	)
}

// Destroy deletes the Worker resource.
func (w *worker) Destroy(ctx context.Context) error {
	return extensions.DeleteExtensionObject(
		ctx,
		w.client,
		w.worker,
	)
}

// Wait waits until the Worker resource is ready.
func (w *worker) Wait(ctx context.Context) error {
	return extensions.WaitUntilExtensionObjectReady(
		ctx,
		w.client,
		w.log,
		w.worker,
		extensionsv1alpha1.WorkerResource,
		w.waitInterval,
		w.waitSevereThreshold,
		w.waitTimeout,
		nil,
	)
}

// WaitUntilWorkerStatusMachineDeploymentsUpdated waits until the worker status is updated with the latest machineDeployment slice.
func (w *worker) WaitUntilWorkerStatusMachineDeploymentsUpdated(ctx context.Context) error {
	return extensions.WaitUntilObjectReadyWithHealthFunction(
		ctx,
		w.client,
		w.log,
		w.checkWorkerStatusMachineDeploymentsUpdated,
		w.worker,
		extensionsv1alpha1.WorkerResource,
		w.waitInterval,
		w.waitSevereThreshold,
		w.waitTimeout,
		func() error {
			w.machineDeployments = w.worker.Status.MachineDeployments
			return nil
		},
	)
}

// WaitMigrate waits until the Worker resources are migrated successfully.
func (w *worker) WaitMigrate(ctx context.Context) error {
	return extensions.WaitUntilExtensionObjectMigrated(
		ctx,
		w.client,
		w.worker,
		extensionsv1alpha1.WorkerResource,
		w.waitInterval,
		w.waitTimeout,
	)
}

// WaitCleanup waits until the Worker resource is deleted.
func (w *worker) WaitCleanup(ctx context.Context) error {
	return extensions.WaitUntilExtensionObjectDeleted(
		ctx,
		w.client,
		w.log,
		w.worker,
		extensionsv1alpha1.WorkerResource,
		w.waitInterval,
		w.waitTimeout,
	)
}

// SetSSHPublicKey sets the public SSH key in the values.
func (w *worker) SetSSHPublicKey(key []byte) {
	w.values.SSHPublicKey = key
}

// SetInfrastructureProviderStatus sets the infrastructure provider status in the values.
func (w *worker) SetInfrastructureProviderStatus(status *runtime.RawExtension) {
	w.values.InfrastructureProviderStatus = status
}

// SetWorkerNameToOperatingSystemConfigsMap sets the operating system config maps in the values.
func (w *worker) SetWorkerNameToOperatingSystemConfigsMap(maps map[string]*operatingsystemconfig.OperatingSystemConfigs) {
	w.values.WorkerNameToOperatingSystemConfigsMap = maps
}

// MachineDeployments returns the generated machine deployments of the Worker.
func (w *worker) MachineDeployments() []extensionsv1alpha1.MachineDeployment {
	return w.machineDeployments
}

func (w *worker) findNodeTemplateAndMachineTypeByPoolName(obj *extensionsv1alpha1.Worker, poolName string) (*extensionsv1alpha1.NodeTemplate, string) {
	for _, pool := range obj.Spec.Pools {
		if pool.Name == poolName {
			return pool.NodeTemplate, pool.MachineType
		}
	}
	return nil, ""
}

// checkWorkerStatusMachineDeploymentsUpdated checks if the status of the worker is updated or not during its reconciliation.
// It is updated if
// * The status.MachineDeploymentsLastUpdateTime > the value of the time stamp stored in worker struct before the reconciliation begins.
func (w *worker) checkWorkerStatusMachineDeploymentsUpdated(o client.Object) error {
	obj, ok := o.(*extensionsv1alpha1.Worker)
	if !ok {
		return fmt.Errorf("expected *extensionsv1alpha1.Worker but got %T", o)
	}

	if obj.Status.MachineDeploymentsLastUpdateTime != nil && (w.machineDeploymentsLastUpdateTime == nil || obj.Status.MachineDeploymentsLastUpdateTime.After(w.machineDeploymentsLastUpdateTime.Time)) {
		return nil
	}

	return fmt.Errorf("worker status machineDeployments has not been updated")
}
