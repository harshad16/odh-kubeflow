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

package controllers

import (
	"context"
	"reflect"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// ViewRoleBindingName is the name of the role binding that grants view access to the notebook
	ViewRoleBindingName = "-view-datasciencecluster"
)

// NewRoleBinding defines the desired role binding object
func NewRoleBinding(notebook *nbv1.Notebook) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + ViewRoleBindingName,
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      notebook.Name,
				Namespace: notebook.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "datascienceclusters.datasciencecluster.opendatahub.io-v1-view",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
}

// reconcileRoleBinding will manage the creation, update and deletion of the
// role binding when the notebook is reconciled
func (r *OpenshiftNotebookReconciler) reconcileRoleBinding(notebook *nbv1.Notebook,
	ctx context.Context, newRoleBinding func(notebook *nbv1.Notebook) *rbacv1.RoleBinding) error {
	// Initialize the logger
	log := r.Log.WithValues("notebook", types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace})

	// Define a new RoleBinding object
	roleBinding := newRoleBinding(notebook)

	// Check if the RoleBinding already exists
	found := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      notebook.Name + ViewRoleBindingName,
		Namespace: notebook.Namespace,
	}, found)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating RoleBinding", "RoleBinding.Namespace", roleBinding.Namespace, "RoleBinding.Name", roleBinding.Name)
		err = r.Create(ctx, roleBinding)
		if err != nil {
			log.Error(err, "Failed to create RoleBinding", "RoleBinding.Namespace", roleBinding.Namespace, "RoleBinding.Name", roleBinding.Name)
			return err
		}
		return nil
	} else if err != nil {
		log.Error(err, "Failed to get RoleBinding")
		return err
	}

	if !reflect.DeepEqual(roleBinding.Subjects, found.Subjects) {
		log.Info("Updating RoleBinding", "RoleBinding.Namespace", roleBinding.Namespace, "RoleBinding.Name", roleBinding.Name)
		err = r.Update(ctx, roleBinding)
		if err != nil {
			log.Error(err, "Failed to update RoleBinding", "RoleBinding.Namespace", roleBinding.Namespace, "RoleBinding.Name", roleBinding.Name)
			return err
		}
	}

	return nil
}

// Reconcile will manage the creation, update and deletion of the role binding
// when the notebook is reconciled
func (r *OpenshiftNotebookReconciler) ReconcileRoleBinding(
	notebook *nbv1.Notebook, ctx context.Context) error {
	return r.reconcileRoleBinding(notebook, ctx, NewRoleBinding)
}
