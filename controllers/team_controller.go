/*
Copyright 2023.

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

	teamv1alpha1 "github.com/snapp-incubator/team-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	MetricNamespaceSuffix    = "-team"
	MetricNamespaceFinalizer = "batch.finalizers.kubebuilder.io/metric-namespace"
	teamFinalizer            = "team.snappcloud.io/cleanup-team"
)

// TeamReconciler reconciles a Team object
type TeamReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=team.snappcloud.io,resources=teams,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=team.snappcloud.io,resources=teams/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=team.snappcloud.io,resources=teams/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=namespaces/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=namespaces/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Team object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (t *TeamReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	team := &teamv1alpha1.Team{}

	err := t.Client.Get(ctx, req.NamespacedName, team)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("team resource not found. Ignoring since the object must be deleted", "team", req.NamespacedName)
			err = t.checkMetricNSForTeamIsDeleted(ctx, req)
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get team", "team", req.NamespacedName)
		return ctrl.Result{}, err
	}

	if !team.ObjectMeta.GetDeletionTimestamp().IsZero() {
		err = t.checkMetricNSForTeamIsDeleted(ctx, req)
		if err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if controllerutil.ContainsFinalizer(team, MetricNamespaceFinalizer) {
			controllerutil.RemoveFinalizer(team, MetricNamespaceFinalizer)
			if err = t.Client.Update(ctx, team); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	namespace := &corev1.Namespace{}
	_ = t.Client.Get(ctx, types.NamespacedName{Name: namespace.Name}, namespace)

	teamName := team.GetName()

	errAddFinalizer := t.CheckMetricNSFinalizerIsAdded(ctx, team)
	if errAddFinalizer != nil {
		return ctrl.Result{}, errAddFinalizer
	}

	metricTeamErr := t.CheckMetricNSForTeamIsCreated(ctx, req)
	if metricTeamErr != nil {
		return ctrl.Result{}, metricTeamErr
	}

	// adding team label for each namespace in team spec
	for _, ns := range team.Spec.Namespaces {
		namespace := &corev1.Namespace{}

		err := t.Client.Get(ctx, types.NamespacedName{Name: ns}, namespace)
		if err != nil {
			log.Error(err, "failed to get namespace", "namespace", ns)
			return ctrl.Result{}, err
		}
		namespace.Labels["snappcloud.io/team"] = teamName
		namespace.Labels["snappcloud.io/datasource"] = "true"
		// Add finalizer to Namespace resource
		// Add finalizer to Namespace resource if not present
		if namespace.ObjectMeta.DeletionTimestamp.IsZero() {
			// The object is not being deleted, so if it does not have our finalizer,
			// then lets add the finalizer and update the object. This is equivalent
			// to registering our finalizer.
			if !controllerutil.ContainsFinalizer(namespace, teamFinalizer) {
				controllerutil.AddFinalizer(namespace, teamFinalizer)
				if err := t.Update(ctx, namespace); err != nil {
					return ctrl.Result{}, err
				}
			}
		} else {
			// The object is being deleted
			if controllerutil.ContainsFinalizer(namespace, teamFinalizer) {
				// our finalizer is present, so lets handle any external dependency
				if err := t.finalizeNamespace(ctx, req, namespace, team); err != nil {
					// if fail to delete the external dependency here, return with error
					// so that it can be retried.
					return ctrl.Result{}, err
				}

				// remove our finalizer from the list and update it.
				controllerutil.RemoveFinalizer(namespace, teamFinalizer)
				if err := t.Update(ctx, namespace); err != nil {
					return ctrl.Result{}, err
				}
			}

			// Stop reconciliation as the item is being deleted
			return ctrl.Result{}, nil
		}

		if err = controllerutil.SetControllerReference(team, namespace, t.Scheme); err != nil {
			log.Error(err, "Failed to add owner refrence")
			return ctrl.Result{}, err
		}

		err = t.Client.Update(ctx, namespace)
		if err != nil {
			log.Error(err, "failed to update namespace", "namespace", ns)
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (t *TeamReconciler) CheckMetricNSFinalizerIsAdded(ctx context.Context, team *teamv1alpha1.Team) error {
	if !controllerutil.ContainsFinalizer(team, MetricNamespaceFinalizer) {
		controllerutil.AddFinalizer(team, MetricNamespaceFinalizer)
		err := t.Client.Update(ctx, team)
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *TeamReconciler) CheckMetricNSForTeamIsCreated(ctx context.Context, req ctrl.Request) error {
	metricTeamNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Name + MetricNamespaceSuffix,
		},
	}
	err := t.Client.Create(ctx, metricTeamNS)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func (t *TeamReconciler) checkMetricNSForTeamIsDeleted(ctx context.Context, req ctrl.Request) error {
	metricTeamNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Name + MetricNamespaceSuffix,
		},
	}
	err := t.Client.Delete(ctx, metricTeamNS)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (t *TeamReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Predicate to filter Pods with the label app=toxiproxy

	// labelPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
	// 	labels := obj.GetLabels()
	// 	return labels["snappcloud.io/team"] == "smapp" // Check if the label exists with any value

	// })
	labelPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()["snappcloud.io/team"] == "smapp"
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&teamv1alpha1.Team{}).
		Watches(
			&source.Kind{Type: &corev1.Namespace{}},
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(labelPredicate),
		).
		Complete(t)
}

func (t *TeamReconciler) finalizeNamespace(ctx context.Context, req ctrl.Request, ns *corev1.Namespace, team *teamv1alpha1.Team) error {
	// Remove the namespace from the list in TeamSpec
	for i, namespace := range team.Spec.Namespaces {
		if namespace == ns.Name {
			team.Spec.Namespaces = append(team.Spec.Namespaces[:i], team.Spec.Namespaces[i+1:]...)
			break
		}
	}

	// Update the Team resource with the modified TeamSpec
	if err := controllerutil.SetControllerReference(team, ns, t.Scheme); err != nil {
		return err
	}

	if err := t.Client.Update(ctx, team); err != nil {
		return err
	}

	return nil

}
