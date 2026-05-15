/*
Copyright 2026.

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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	openstackv1 "github.com/JustHumanz/ovs-cni-controller/api/v1"
	neutronOperator "github.com/JustHumanz/ovs-cni-controller/internal/neutron-op"
	"github.com/gophercloud/gophercloud/v2"
	"go.yaml.in/yaml/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// NeutronConfigReconciler reconciles a NeutronConfig object
type NeutronConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	neutronFinalizerName = "openstack.humanz.moe/finalizer"
)

// +kubebuilder:rbac:groups=openstack.humanz.moe,resources=neutronconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openstack.humanz.moe,resources=neutronconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openstack.humanz.moe,resources=neutronconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=openstack.humanz.moe,resources=neutronipaddresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openstack.humanz.moe,resources=neutronipaddresses/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the NeutronConfig object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/reconcile
func (r *NeutronConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	neutronConfig := &openstackv1.NeutronConfig{}

	if err := r.Get(ctx, req.NamespacedName, neutronConfig); err != nil {
		if errors.IsNotFound(err) {
			// Resource not found, could have been deleted after reconcile request. Return and don't requeue.
			return ctrl.Result{RequeueAfter: -1}, nil
		}
		logf.Log.Error(err, "unable to fetch NeutronConfig")
		return ctrl.Result{}, nil
	}

	// Auth OpenStack client
	OSauthCM := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Name: neutronConfig.Spec.OpenStackAuthConfigName, Namespace: req.Namespace}, OSauthCM)
	if err != nil {
		logf.Log.Error(err, "unable to fetch OpenStack auth config ConfigMap")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clouds := OSauthCM.Data["clouds.yaml"]
	var osAuthConfig neutronOperator.OSAuthConfig
	err = yaml.Unmarshal([]byte(clouds), &osAuthConfig)
	if err != nil {
		logf.Log.Error(err, "unable to parse OpenStack auth config")
		return ctrl.Result{}, err
	}

	if neutronConfig.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(neutronConfig, neutronFinalizerName) {
			controllerutil.AddFinalizer(neutronConfig, neutronFinalizerName)
			if err := r.Update(ctx, neutronConfig); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(neutronConfig, neutronFinalizerName) {
			neutronClient, err := neutronOperator.NeutronInit(neutronConfig, osAuthConfig, l)
			if err != nil {
				logf.Log.Error(err, "unable to initialize OpenStack client for cleanup")
				return ctrl.Result{}, err
			}

			// our finalizer is present, so let's handle any external dependency
			if err := r.deleteExternalResources(neutronConfig, neutronClient); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried.
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(neutronConfig, neutronFinalizerName)
			if err := r.Update(ctx, neutronConfig); err != nil {
				return ctrl.Result{}, err
			}
		}

		logf.Log.Info("NeutronConfig resource is being deleted, external resources cleaned up successfully")

		return ctrl.Result{}, nil
	}

	// Init the OpenStack client
	neutronClient, err := neutronOperator.NeutronInit(neutronConfig, osAuthConfig, l)
	if err != nil {
		logf.Log.Error(err, "unable to initialize OpenStack client")
		r.setCondition(neutronConfig, "Degraded", metav1.ConditionFalse, "Unhealthy", err.Error())
		if statusErr := r.Status().Update(ctx, neutronConfig); statusErr != nil {
			logf.Log.Error(statusErr, "unable to update NeutronConfig status")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{
			RequeueAfter: 10 * time.Second,
		}, err
	}

	neutron_ports, err := neutronOperator.NeutronCreatePorts(neutronConfig, neutronClient, l)
	if err != nil {
		logf.Log.Error(err, "unable to create Neutron ports")
		r.setCondition(neutronConfig, "Degraded", metav1.ConditionFalse, "Unhealthy", err.Error())
		if statusErr := r.Status().Update(ctx, neutronConfig); statusErr != nil {
			logf.Log.Error(statusErr, "unable to update NeutronConfig status")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	for _, v := range neutron_ports {
		neutronIpPool := &openstackv1.NeutronIPAddress{}
		neutronIpPool.ObjectMeta.Name = "neutron-ip-" + v.FixedIPs[0].IPAddress
		neutronIpPool.ObjectMeta.Namespace = req.Namespace

		// Check if the port have correct tag
		if len(v.Tags) != 2 {
			return ctrl.Result{}, fmt.Errorf("Port tags are missing: %v", v.Tags)
		}

		// create new
		ipSubNet := strings.Split(v.Tags[0], "=")[1]
		gateWay := strings.Split(v.Tags[1], "=")[1]
		neutronIpPool.Spec = openstackv1.NeutronIPAddressSpec{
			IpAddress:  v.FixedIPs[0].IPAddress,
			Subnet:     ipSubNet,
			MacAddress: v.MACAddress,
			PortID:     v.ID,
		}

		neutronIpPool.ObjectMeta.Annotations = map[string]string{
			"openstack.humanz.moe/neutronConfig": neutronConfig.Name,
		}

		neutronIpPool.ObjectMeta.Labels = map[string]string{
			"state":   "unbound",
			"network": neutronConfig.Spec.NetworkUUID,
			"gateway": gateWay,
		}

		err = r.Create(ctx, neutronIpPool)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	logf.Log.Info("Successfully created Neutron ports", "portCount", len(neutron_ports))

	r.setCondition(neutronConfig, "Available", metav1.ConditionTrue, "Healthy", "Successfully created Neutron ports")
	if statusErr := r.Update(ctx, neutronConfig); statusErr != nil {
		logf.Log.Error(statusErr, "unable to update NeutronConfig status")
		return ctrl.Result{}, statusErr
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NeutronConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openstackv1.NeutronConfig{}).
		Named("neutronconfig").
		Complete(r)
}

func (r *NeutronConfigReconciler) setCondition(
	neutron *openstackv1.NeutronConfig,
	condType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) {
	meta.SetStatusCondition(&neutron.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: neutron.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func (r *NeutronConfigReconciler) deleteExternalResources(neutronConfig *openstackv1.NeutronConfig, neutronClient *gophercloud.ServiceClient) error {
	naddr := &openstackv1.NeutronIPAddressList{}
	if err := r.List(context.Background(), naddr, client.InNamespace(neutronConfig.Namespace), client.MatchingLabels{"network": neutronConfig.Spec.NetworkUUID}); err != nil {
		return err
	}

	for _, nport := range naddr.Items {
		if strings.EqualFold(nport.Annotations["openstack.humanz.moe/neutronConfig"], neutronConfig.Name) {
			// Deleting the Neutron port associated with this IP address
			err := neutronOperator.NeutronDeletePort(nport.Spec.PortID, neutronClient)
			if err != nil {
				return err
			}

			if err := r.Delete(context.Background(), &nport); err != nil {
				return err
			}
		}
	}
	return nil
}
