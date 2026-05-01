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

	"go.yaml.in/yaml/v2"
	openstackv1 "humanz.moe/kube-ovs/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// NeutronConfigReconciler reconciles a NeutronConfig object
type NeutronConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

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

	err := r.Get(ctx, req.NamespacedName, neutronConfig)
	if err != nil {
		logf.Log.Error(err, "unable to fetch NeutronConfig")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	OSauthCM := &corev1.ConfigMap{}
	err = r.Get(ctx, client.ObjectKey{Name: neutronConfig.Spec.OpenStackAuthConfigName, Namespace: req.Namespace}, OSauthCM)
	if err != nil {
		logf.Log.Error(err, "unable to fetch OpenStack auth config ConfigMap")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clouds := OSauthCM.Data["clouds.yaml"]
	var osAuthConfig OSAuthConfig
	err = yaml.Unmarshal([]byte(clouds), &osAuthConfig)
	if err != nil {
		logf.Log.Error(err, "unable to parse OpenStack auth config")
		return ctrl.Result{}, err
	}

	neutronClient, err := r.NeutronInit(neutronConfig, osAuthConfig, l)
	if err != nil {
		meta.SetStatusCondition(&neutronConfig.Status.Conditions, metav1.Condition{
			Type:               "Degraded",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: neutronConfig.Generation,
			Reason:             "ReconcileFailed",
			Message:            "Failed to reconcile the resource due to an error in OpenStack initialization: " + err.Error(),
		})
		if statusErr := r.Status().Update(ctx, neutronConfig); statusErr != nil {
			logf.Log.Error(statusErr, "unable to update NeutronConfig status")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	meta.SetStatusCondition(&neutronConfig.Status.Conditions, metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: neutronConfig.Generation,
		Reason:             "Reconciled",
		Message:            "NeutronConfig successfully initialization with OpenStack",
	})

	desiredIPs := map[string]struct{}{}
	for _, ip := range printIPList(neutronConfig.Spec.Ips) {
		desiredIPs[ip] = struct{}{}
	}

	neutron_ports, err := r.NeutronCreatePorts(neutronConfig, neutronClient, l)
	if err != nil {
		logf.Log.Error(err, "unable to create port in OpenStack")
		meta.SetStatusCondition(&neutronConfig.Status.Conditions, metav1.Condition{
			Type:               "Degraded",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: neutronConfig.Generation,
			Reason:             "ReconcileFailed",
			Message:            "Failed to reconcile the resource due to an error in Neutron port creation: " + err.Error(),
		})
		if statusErr := r.Status().Update(ctx, neutronConfig); statusErr != nil {
			logf.Log.Error(statusErr, "unable to update NeutronConfig status")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	meta.SetStatusCondition(&neutronConfig.Status.Conditions, metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: neutronConfig.Generation,
		Reason:             "Reconciled",
		Message:            "NeutronConfig successfully created ports in OpenStack",
	})

	for _, v := range neutron_ports {
		neutronIpPool := &openstackv1.NeutronIPAddress{}
		neutronIpPool.ObjectMeta.Name = "neutron-ip-" + v.FixedIPs[0].IPAddress
		neutronIpPool.ObjectMeta.Namespace = req.Namespace

		err = r.Get(ctx, client.ObjectKeyFromObject(neutronIpPool), neutronIpPool)
		if err != nil && client.IgnoreNotFound(err) != nil {
			logf.Log.Error(err, "unable to check NeutronIPAddress resource existence", "name", neutronIpPool.ObjectMeta.Name)
			meta.SetStatusCondition(&neutronConfig.Status.Conditions, metav1.Condition{
				Type:               "Degraded",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: neutronConfig.Generation,
				Reason:             "ReconcileFailed",
				Message:            "Failed to check NeutronIPAddress resource: " + err.Error(),
			})
			if statusErr := r.Status().Update(ctx, neutronConfig); statusErr != nil {
				logf.Log.Error(statusErr, "unable to update NeutronConfig status")
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
		if err == nil {
			// already exists, skip
			continue
		}

		// create new
		neutronIpPool.Spec = openstackv1.NeutronIPAddressSpec{
			IpAddress:  v.FixedIPs[0].IPAddress,
			MacAddress: v.MACAddress,
			PortID:     v.ID,
		}
		neutronIpPool.ObjectMeta.Labels = map[string]string{
			"state":   "unbound",
			"network": neutronConfig.Spec.NetworkUUID,
		}

		err = r.Create(ctx, neutronIpPool)
		if err != nil {
			logf.Log.Error(err, "unable to create NeutronIPAddress resource for port", "portID", v.ID)
			meta.SetStatusCondition(&neutronConfig.Status.Conditions, metav1.Condition{
				Type:               "Degraded",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: neutronConfig.Generation,
				Reason:             "ReconcileFailed",
				Message:            "Failed to create NeutronIPAddress resource: " + err.Error(),
			})
			if statusErr := r.Status().Update(ctx, neutronConfig); statusErr != nil {
				logf.Log.Error(statusErr, "unable to update NeutronConfig status")
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
	}

	existingIps := &openstackv1.NeutronIPAddressList{}
	if err := r.List(ctx, existingIps, client.InNamespace(req.Namespace), client.MatchingLabels{"network": neutronConfig.Spec.NetworkUUID}); err != nil {
		logf.Log.Error(err, "unable to list NeutronIPAddress resources for cleanup")
		meta.SetStatusCondition(&neutronConfig.Status.Conditions, metav1.Condition{
			Type:               "Degraded",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: neutronConfig.Generation,
			Reason:             "ReconcileFailed",
			Message:            "Failed to list NeutronIPAddress resources for cleanup: " + err.Error(),
		})
		if statusErr := r.Status().Update(ctx, neutronConfig); statusErr != nil {
			logf.Log.Error(statusErr, "unable to update NeutronConfig status")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	for _, item := range existingIps.Items {
		if _, keep := desiredIPs[item.Spec.IpAddress]; keep {
			continue
		}

		if err := r.Delete(ctx, item.DeepCopy()); err != nil && client.IgnoreNotFound(err) != nil {
			logf.Log.Error(err, "unable to delete stale NeutronIPAddress resource", "name", item.Name)
			meta.SetStatusCondition(&neutronConfig.Status.Conditions, metav1.Condition{
				Type:               "Degraded",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: neutronConfig.Generation,
				Reason:             "ReconcileFailed",
				Message:            "Failed to delete stale NeutronIPAddress resource: " + err.Error(),
			})
			if statusErr := r.Status().Update(ctx, neutronConfig); statusErr != nil {
				logf.Log.Error(statusErr, "unable to update NeutronConfig status")
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}

		if err := r.NeutronDeletePort(item.Spec.PortID, neutronClient, l); err != nil {
			logf.Log.Error(err, "unable to delete stale Neutron port in OpenStack", "portID", item.Spec.PortID)
			meta.SetStatusCondition(&neutronConfig.Status.Conditions, metav1.Condition{
				Type:               "Degraded",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: neutronConfig.Generation,
				Reason:             "ReconcileFailed",
				Message:            "Failed to delete stale Neutron port in OpenStack: " + err.Error(),
			})
			if statusErr := r.Status().Update(ctx, neutronConfig); statusErr != nil {
				logf.Log.Error(statusErr, "unable to update NeutronConfig status")
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
	}

	meta.SetStatusCondition(&neutronConfig.Status.Conditions, metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: neutronConfig.Generation,
		Reason:             "Healthy",
		Message:            "NeutronConfig successfully created NeutronIPAddress resources in Kubernetes",
	})

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NeutronConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openstackv1.NeutronConfig{}).
		Named("neutronconfig").
		Complete(r)
}
