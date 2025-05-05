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
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	elyraRuntimeSecretName = "ds-pipeline-config"
	elyraRuntimeMountPath  = "/opt/app-root/runtimes"
	elyraRuntimeVolumeName = "elyra-dsp-details"
)

// extractElyraRuntimeConfigInfo retrieves the essential configuration details from dspa and dashboard CRs used for pipeline execution.
func extractElyraRuntimeConfigInfo(ctx context.Context, dynamicClient dynamic.Interface, client client.Client, notebook *nbv1.Notebook, log logr.Logger) (map[string]interface{}, error) {
	// Define GVRs
	dspa := schema.GroupVersionResource{
		Group:    "datasciencepipelinesapplications.opendatahub.io",
		Version:  "v1",
		Resource: "datasciencepipelinesapplications",
	}
	dashboard := schema.GroupVersionResource{
		Group:    "components.platform.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "dashboards",
	}

	// Fetch DSPA CR
	dspaObj, err := dynamicClient.Resource(dspa).Namespace(notebook.Namespace).Get(ctx, "dspa", metav1.GetOptions{})
	if err != nil {
		// DSPA CR not found; skipping Elyra config generation
		if apierrs.IsNotFound(err) {
			return nil, nil
		}
		log.Error(err, "Failed to get DSPA CR")
		return nil, fmt.Errorf("error retrieving DSPA CR: %w", err)
	}

	// Fetch Dashboard CR
	dashboardObj, err := dynamicClient.Resource(dashboard).Get(ctx, "default-dashboard", metav1.GetOptions{})
	if err != nil {
		log.Error(err, "Failed to get Dashboard CR")
		return nil, fmt.Errorf("error retrieving Dashboard CR: %w", err)
	}

	// Extract dashboard URL
	status, ok := dashboardObj.Object["status"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid Dashboard CR: missing 'status'")
	}
	dashboardURL, ok := status["url"].(string)
	if !ok || dashboardURL == "" {
		return nil, fmt.Errorf("invalid Dashboard CR: missing or empty 'url'")
	}
	publicAPIEndpoint := fmt.Sprintf("https://%s/experiments/%s/", dashboardURL, notebook.Namespace)

	// Extract info from DSPA spec
	spec, ok := dspaObj.Object["spec"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'spec'")
	}
	objectStorage, ok := spec["objectStorage"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'objectStorage'")
	}
	externalStorage, ok := objectStorage["externalStorage"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'externalStorage'")
	}

	// Extract host
	host, ok := externalStorage["host"].(string)
	if !ok || host == "" {
		return nil, fmt.Errorf("invalid DSPA CR: missing or invalid 'host'")
	}
	cosEndpoint := fmt.Sprintf("https://%s", host)

	// Extract bucket
	cosBucket, ok := externalStorage["bucket"].(string)
	if !ok || cosBucket == "" {
		return nil, fmt.Errorf("invalid DSPA CR: missing or invalid 'bucket'")
	}

	// Extract S3 credentials
	s3CredentialsSecret, ok := externalStorage["s3CredentialsSecret"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 's3CredentialsSecret'")
	}
	cosSecret, ok := s3CredentialsSecret["secretName"].(string)
	usernameKey, ok1 := s3CredentialsSecret["accessKey"].(string)
	passwordKey, ok2 := s3CredentialsSecret["secretKey"].(string)
	if !ok || !ok1 || !ok2 {
		return nil, fmt.Errorf("invalid DSPA CR: incomplete 's3CredentialsSecret'")
	}

	// Fetch secret for credentials
	dashboardSecret := &corev1.Secret{}
	err = client.Get(ctx, types.NamespacedName{Name: cosSecret, Namespace: notebook.Namespace}, dashboardSecret)
	if err != nil {
		log.Error(err, "Failed to get secret", "secretName", cosSecret)
		return nil, fmt.Errorf("failed to get secret '%s': %w", cosSecret, err)
	}

	// Extract values from the secret
	usernameVal, ok := dashboardSecret.Data[usernameKey]
	if !ok {
		return nil, fmt.Errorf("missing key '%s' in secret '%s'", usernameKey, cosSecret)
	}
	passwordVal, ok := dashboardSecret.Data[passwordKey]
	if !ok {
		return nil, fmt.Errorf("missing key '%s' in secret '%s'", passwordKey, cosSecret)
	}

	cosUsername := string(usernameVal)
	cosPassword := string(passwordVal)

	// Extract API Endpoint from DSPA status
	status, ok = dspaObj.Object["status"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'status'")
	}
	components, ok := status["components"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'components' in status")
	}
	apiServer, ok := components["apiServer"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'apiServer' in components")
	}
	apiEndpoint, ok := apiServer["externalUrl"].(string)
	if !ok || apiEndpoint == "" {
		return nil, fmt.Errorf("invalid DSPA CR: missing or invalid 'externalUrl' for apiServer")
	}

	// Construct and return the DSPA config
	return map[string]interface{}{
		"display_name": "Data Science Pipeline",
		"schema_name":  "kfp",
		"metadata": map[string]interface{}{
			"tags":                []string{},
			"display_name":        "Data Science Pipeline",
			"engine":              "Argo",
			"runtime_type":        "KUBEFLOW_PIPELINES",
			"auth_type":           "KUBERNETES_SERVICE_ACCOUNT_TOKEN",
			"cos_auth_type":       "KUBERNETES_SECRET",
			"public_api_endpoint": publicAPIEndpoint,
			"api_endpoint":        apiEndpoint,
			"cos_endpoint":        cosEndpoint,
			"cos_bucket":          cosBucket,
			"cos_username":        cosUsername,
			"cos_password":        cosPassword,
			"cos_secret":          cosSecret,
		},
	}, nil
}

// NewElyraRuntimeConfigSecret defines and handles the creation, watch and update to the desired ElyraRuntimeConfig secret object
func (r *OpenshiftNotebookReconciler) NewElyraRuntimeConfigSecret(ctx context.Context, dynamicConfig *rest.Config, c client.Client, notebook *nbv1.Notebook, controllerNamespace string, log logr.Logger) error {
	dynamicClient, err := dynamic.NewForConfig(dynamicConfig)
	if err != nil {
		log.Error(err, "Failed to create dynamic client")
		return err
	}

	dspData, err := extractElyraRuntimeConfigInfo(ctx, dynamicClient, c, notebook, log)
	if err != nil {
		log.Error(err, "Failed to extract Elyra runtime config info")
		return err
	}
	// // In case No DSPA present in namespace skipping Elyra secret creation as DSPA is not present
	if dspData == nil {
		return nil
	}

	dspJSON, err := json.Marshal(dspData)
	if err != nil {
		log.Error(err, "Failed to marshal DSPA config to JSON")
		return err
	}

	desiredSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      elyraRuntimeSecretName,
			Namespace: notebook.Namespace,
			Labels:    map[string]string{"opendatahub.io/managed-by": "workbenches"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"odh_dsp.json": dspJSON,
		},
	}

	// Try to fetch existing secret
	existingSecret := &corev1.Secret{}
	err = c.Get(ctx, types.NamespacedName{Name: elyraRuntimeSecretName, Namespace: notebook.Namespace}, existingSecret)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating Elyra runtime config secret", "name", elyraRuntimeSecretName)
			// Patch the secret with proper field ownership
			desiredSecret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
			patchData, err := json.Marshal(desiredSecret)
			if err != nil {
				log.Error(err, "Failed to marshal desired secret for apply")
				return err
			}
			if err := c.Patch(ctx, desiredSecret, client.RawPatch(types.ApplyPatchType, patchData),
				client.ForceOwnership, client.FieldOwner("dspa")); err != nil {
				log.Error(err, "Failed to apply Elyra runtime config secret")
				return err
			}

			if err := c.Create(ctx, desiredSecret); err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Failed to create Elyra runtime config secret")
				return err
			}
			return nil
		}
		log.Error(err, "Failed to get Elyra runtime config secret")
		return err
	}

	// Always reconcile label and data
	requiresUpdate := !reflect.DeepEqual(existingSecret.Data, desiredSecret.Data) ||
		existingSecret.Labels["opendatahub.io/managed-by"] != "workbenches"

	if requiresUpdate {
		log.Info("Overriding existing Elyra runtime config secret", "name", elyraRuntimeSecretName)

		// Set correct label and data
		existingSecret.Labels = map[string]string{"opendatahub.io/managed-by": "workbenches"}
		existingSecret.Data = desiredSecret.Data

		if err := c.Update(ctx, existingSecret); err != nil {
			log.Error(err, "Failed to override existing Elyra runtime config secret")
			return err
		}
	}

	return nil
}

// MountElyraRuntimeConfigSecret injects the Elyra runtime configuration Secret as a volume mount into the Notebook pod.
// This function is invoked by the webhook during Notebook mutation.
func MountElyraRuntimeConfigSecret(ctx context.Context, client client.Client, notebook *nbv1.Notebook, log logr.Logger) error {

	// Retrieve the Secret
	secret := &corev1.Secret{}
	err := client.Get(ctx, types.NamespacedName{Name: elyraRuntimeSecretName, Namespace: notebook.Namespace}, secret)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Secret is not available yet", "Secret", elyraRuntimeSecretName)
			return nil
		}
		log.Error(err, "Error retrieving Secret", "Secret", elyraRuntimeSecretName)
		return err
	}

	// Check that it's our managed secret and has expected data
	if secret.Labels["opendatahub.io/managed-by"] != "workbenches" {
		log.Info("Skipping mounting secret not managed by workbenches", "Secret", elyraRuntimeSecretName)
		return nil
	}
	if len(secret.Data) == 0 {
		log.Info("Secret is empty, skipping volume mount", "Secret", elyraRuntimeSecretName)
		return nil
	}

	// Define the volume
	secretVolume := corev1.Volume{
		Name: elyraRuntimeVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: elyraRuntimeSecretName,
				Optional:   ptr.To(true),
			},
		},
	}

	// Append the volume if it doesn't already exist
	volumes := &notebook.Spec.Template.Spec.Volumes
	volumeExists := false
	for _, v := range *volumes {
		if v.Name == elyraRuntimeVolumeName {
			volumeExists = true
			break
		}
	}
	if !volumeExists {
		*volumes = append(*volumes, secretVolume)
		log.Info("Added elyra-dsp-details volume to notebook", "notebook", notebook.Name, "namespace", notebook.Namespace)
	} else {
		log.Info("elyra-dsp-details volume already exists, skipping", "notebook", notebook.Name, "namespace", notebook.Namespace)
	}

	log.Info("Injecting elyra-dsp-details volume into notebook", "notebook", notebook.Name, "namespace", notebook.Namespace)

	// Append volume mount to container (ensure no duplication by name or mountPath)
	for i, container := range notebook.Spec.Template.Spec.Containers {
		mountExists := false
		for _, vm := range container.VolumeMounts {
			if vm.Name == elyraRuntimeVolumeName || vm.MountPath == elyraRuntimeMountPath {
				mountExists = true
				break
			}
		}
		if !mountExists {
			notebook.Spec.Template.Spec.Containers[i].VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      elyraRuntimeVolumeName,
				MountPath: elyraRuntimeMountPath,
			})
			log.Info("Added elyra-dsp-details volume mount", "container", container.Name, "mountPath", elyraRuntimeMountPath)
		} else {
			log.Info("elyra-dsp-details volume mount already exists, skipping", "container", container.Name, "mountPath", elyraRuntimeMountPath)
		}
	}

	return nil
}

// ReconcileElyraRuntimeConfigSecret handles the reconciliation of the Elyra runtime config secret.
// This function is invoked by the ODH Notebook Controller and is required for enabling Elyra functionality in notebooks.
func (r *OpenshiftNotebookReconciler) ReconcileElyraRuntimeConfigSecret(notebook *nbv1.Notebook, ctx context.Context) error {
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)
	return r.NewElyraRuntimeConfigSecret(ctx, r.Config, r.Client, notebook, r.Namespace, log)
}
