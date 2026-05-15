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
	"time"

	openstackv1 "github.com/JustHumanz/ovs-cni-controller/api/v1"
	neutronOperator "github.com/JustHumanz/ovs-cni-controller/internal/neutron-op"
	"github.com/JustHumanz/ovs-cni-controller/internal/utils"
	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"go.yaml.in/yaml/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// NeutronIPAddressReconciler reconciles a NeutronIPAddress object
type NeutronIPAddressReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=openstack.humanz.moe,resources=neutronipaddresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openstack.humanz.moe,resources=neutronipaddresses/status,verbs=get;update;patch
func (r *NeutronIPAddressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	NipAddr := &openstackv1.NeutronIPAddress{}

	if err := r.Get(ctx, req.NamespacedName, NipAddr); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		log.Error(err, "unable to fetch NeutronIPAddress")
		return ctrl.Result{}, err
	}

	switch NipAddr.Labels["state"] {
	case "bound":
		log.Info("IP address is bound", "name", req.NamespacedName)
		neutronClient, err := r.InitNeutronClient(ctx, req, NipAddr, log)
		if err != nil {
			return ctrl.Result{
				RequeueAfter: 10 * time.Second,
			}, err
			// If the client initialization fails, we should requeue the request to retry later
			// This is important because the failure might be transient (e.g., temporary network issue)
			// and we want to give it another chance to succeed without losing the event
		}

		newID := fmt.Sprintf("%s:pods/%s", NipAddr.Namespace, NipAddr.Annotations["openstack.humanz.moe/podName"])
		prt, err := neutronOperator.NeutronUpdatePort(NipAddr.Spec.PortID, neutronClient, ports.UpdateOpts{
			DeviceID: utils.StringPtr(newID),
		})
		if err != nil {
			log.Error(err, "unable to update port")
			return ctrl.Result{
				RequeueAfter: 10 * time.Second,
			}, err
		}

		log.Info("Successfully updated Neutron port with device ID", "portID", prt.ID, "deviceID", newID)

		r.setCondition(NipAddr, "Available", metav1.ConditionFalse, "Healthy", "Successfully updated Neutron port")
		if statusErr := r.Status().Update(ctx, NipAddr); statusErr != nil {
			logf.Log.Error(statusErr, "unable to update NeutronIPAddress status")
			return ctrl.Result{}, statusErr
		}

		return ctrl.Result{}, nil
	case "unbound":
		neutronClient, err := r.InitNeutronClient(ctx, req, NipAddr, log)
		if err != nil {
			return ctrl.Result{
				RequeueAfter: 10 * time.Second,
			}, err
			// If the client initialization fails, we should requeue the request to retry later
			// This is important because the failure might be transient (e.g., temporary network issue)
			// and we want to give it another chance to succeed without losing the event
		}

		prt, err := neutronOperator.NeutronUpdatePort(NipAddr.Spec.PortID, neutronClient, ports.UpdateOpts{
			DeviceID: nil,
		})
		if err != nil {
			log.Error(err, "unable to update port")
			return ctrl.Result{
				RequeueAfter: 10 * time.Second,
			}, err
		}

		log.Info("Successfully init/clean Neutron port with device ID", "portID", prt.ID)

		r.setCondition(NipAddr, "Available", metav1.ConditionTrue, "Healthy", "Successfully init/cleaned Neutron port")
		if statusErr := r.Status().Update(ctx, NipAddr); statusErr != nil {
			logf.Log.Error(statusErr, "unable to update NeutronIPAddress status")
			return ctrl.Result{}, statusErr
		}

		return ctrl.Result{}, nil
	default:
		log.Info("IP address state is unknown", "name", req.NamespacedName, "state", NipAddr.Labels["state"])
	}

	log.Info("Reconciled NeutronIPAddress", "name", req.NamespacedName)
	return ctrl.Result{}, nil
}

func (r *NeutronIPAddressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openstackv1.NeutronIPAddress{}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				// New pod is provisioning, so we want to catch the transition from unbound to bound
				return e.ObjectNew.GetLabels()["state"] == "bound" ||
					// The pods are getting deleted, so we want to catch the transition from bound to unbound
					(e.ObjectNew.GetLabels()["state"] == "unbound" && e.ObjectOld.GetLabels()["state"] != "bound")
			},
		}).
		Named("neutronipaddress").
		Complete(r)
}

func (r *NeutronIPAddressReconciler) InitNeutronClient(ctx context.Context, req ctrl.Request, NipAddr *openstackv1.NeutronIPAddress, l logr.Logger) (*gophercloud.ServiceClient, error) {
	neutronConfig := &openstackv1.NeutronConfig{}
	if err := r.Get(ctx, client.ObjectKey{Name: NipAddr.Annotations["openstack.humanz.moe/neutronConfig"], Namespace: req.Namespace}, neutronConfig); err != nil {
		logf.Log.Error(err, "unable to fetch NeutronConfig")
		return nil, err
	}

	// Auth OpenStack client
	OSauthCM := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Name: neutronConfig.Spec.OpenStackAuthConfigName, Namespace: req.Namespace}, OSauthCM)
	if err != nil {
		logf.Log.Error(err, "unable to fetch OpenStack auth config ConfigMap")
		return nil, client.IgnoreNotFound(err)
	}

	clouds := OSauthCM.Data["clouds.yaml"]
	var osAuthConfig neutronOperator.OSAuthConfig
	err = yaml.Unmarshal([]byte(clouds), &osAuthConfig)
	if err != nil {
		logf.Log.Error(err, "unable to parse OpenStack auth config")
		return nil, err
	}

	neutronClient, err := neutronOperator.NeutronInit(neutronConfig, osAuthConfig, l)
	if err != nil {
		logf.Log.Error(err, "unable to initialize OpenStack client")
		return nil, err
	}

	return neutronClient, nil
}

func (r *NeutronIPAddressReconciler) setCondition(
	neutron *openstackv1.NeutronIPAddress,
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
