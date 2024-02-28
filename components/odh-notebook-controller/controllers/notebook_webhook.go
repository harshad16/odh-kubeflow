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
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

//+kubebuilder:webhook:path=/mutate-notebook-v1,mutating=true,failurePolicy=fail,sideEffects=None,groups=kubeflow.org,resources=notebooks,verbs=create;update,versions=v1,name=notebooks.opendatahub.io,admissionReviewVersions=v1

// NotebookWebhook holds the webhook configuration.
type NotebookWebhook struct {
	Client      client.Client
	Decoder     *admission.Decoder
	OAuthConfig OAuthConfig
}

// InjectReconciliationLock injects the kubeflow notebook controller culling
// stop annotation to explicitly start the notebook pod when the ODH notebook
// controller finishes the reconciliation. Otherwise, a race condition may happen
// while mounting the notebook service account pull secret into the pod.
//
// The ODH notebook controller will remove this annotation when the first
// reconciliation is completed (see RemoveReconciliationLock).
func InjectReconciliationLock(meta *metav1.ObjectMeta) error {
	if meta.Annotations != nil {
		meta.Annotations[culler.STOP_ANNOTATION] = AnnotationValueReconciliationLock
	} else {
		meta.SetAnnotations(map[string]string{
			culler.STOP_ANNOTATION: AnnotationValueReconciliationLock,
		})
	}
	return nil
}

// InjectOAuthProxy injects the OAuth proxy sidecar container in the Notebook
// spec
func InjectOAuthProxy(notebook *nbv1.Notebook, oauth OAuthConfig) error {
	// https://pkg.go.dev/k8s.io/api/core/v1#Container
	proxyContainer := corev1.Container{
		Name:            "oauth-proxy",
		Image:           oauth.ProxyImage,
		ImagePullPolicy: corev1.PullAlways,
		Env: []corev1.EnvVar{{
			Name: "NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		}},
		Args: []string{
			"--provider=openshift",
			"--https-address=:8443",
			"--http-address=",
			"--openshift-service-account=" + notebook.Name,
			"--cookie-secret-file=/etc/oauth/config/cookie_secret",
			"--cookie-expire=24h0m0s",
			"--tls-cert=/etc/tls/private/tls.crt",
			"--tls-key=/etc/tls/private/tls.key",
			"--upstream=http://localhost:8888",
			"--upstream-ca=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
			"--email-domain=*",
			"--skip-provider-button",
			`--openshift-sar={"verb":"get","resource":"notebooks","resourceAPIGroup":"kubeflow.org",` +
				`"resourceName":"` + notebook.Name + `","namespace":"$(NAMESPACE)"}`,
		},
		Ports: []corev1.ContainerPort{{
			Name:          OAuthServicePortName,
			ContainerPort: 8443,
			Protocol:      corev1.ProtocolTCP,
		}},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/oauth/healthz",
					Port:   intstr.FromString(OAuthServicePortName),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
			InitialDelaySeconds: 30,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/oauth/healthz",
					Port:   intstr.FromString(OAuthServicePortName),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
			InitialDelaySeconds: 5,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				"cpu":    resource.MustParse("100m"),
				"memory": resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				"cpu":    resource.MustParse("100m"),
				"memory": resource.MustParse("64Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "oauth-config",
				MountPath: "/etc/oauth/config",
			},
			{
				Name:      "tls-certificates",
				MountPath: "/etc/tls/private",
			},
		},
	}

	// Add logout url if logout annotation is present in the notebook
	if notebook.ObjectMeta.Annotations[AnnotationLogoutUrl] != "" {
		proxyContainer.Args = append(proxyContainer.Args,
			"--logout-url="+notebook.ObjectMeta.Annotations[AnnotationLogoutUrl])
	}

	// Add the sidecar container to the notebook
	notebookContainers := &notebook.Spec.Template.Spec.Containers
	proxyContainerExists := false
	for index, container := range *notebookContainers {
		if container.Name == "oauth-proxy" {
			(*notebookContainers)[index] = proxyContainer
			proxyContainerExists = true
			break
		}
	}
	if !proxyContainerExists {
		*notebookContainers = append(*notebookContainers, proxyContainer)
	}

	// Add the OAuth configuration volume:
	// https://pkg.go.dev/k8s.io/api/core/v1#Volume
	notebookVolumes := &notebook.Spec.Template.Spec.Volumes
	oauthVolumeExists := false
	oauthVolume := corev1.Volume{
		Name: "oauth-config",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  notebook.Name + "-oauth-config",
				DefaultMode: pointer.Int32Ptr(420),
			},
		},
	}
	for index, volume := range *notebookVolumes {
		if volume.Name == "oauth-config" {
			(*notebookVolumes)[index] = oauthVolume
			oauthVolumeExists = true
			break
		}
	}
	if !oauthVolumeExists {
		*notebookVolumes = append(*notebookVolumes, oauthVolume)
	}

	// Add the TLS certificates volume:
	// https://pkg.go.dev/k8s.io/api/core/v1#Volume
	tlsVolumeExists := false
	tlsVolume := corev1.Volume{
		Name: "tls-certificates",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  notebook.Name + "-tls",
				DefaultMode: pointer.Int32Ptr(420),
			},
		},
	}
	for index, volume := range *notebookVolumes {
		if volume.Name == "tls-certificates" {
			(*notebookVolumes)[index] = tlsVolume
			tlsVolumeExists = true
			break
		}
	}
	if !tlsVolumeExists {
		*notebookVolumes = append(*notebookVolumes, tlsVolume)
	}

	// Set a dedicated service account, do not use default
	notebook.Spec.Template.Spec.ServiceAccountName = notebook.Name
	return nil
}

// Handle transforms the Notebook objects.
func (w *NotebookWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	notebook := &nbv1.Notebook{}

	err := w.Decoder.Decode(req, notebook)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Inject the reconciliation lock only on new notebook creation
	if req.Operation == admissionv1.Create {
		err = InjectReconciliationLock(&notebook.ObjectMeta)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

		err = CheckAndMountCACertBundle(ctx, w.Client, notebook)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}

	// Inject the OAuth proxy if the annotation is present but only if Service Mesh is disabled
	if OAuthInjectionIsEnabled(notebook.ObjectMeta) {
		if ServiceMeshIsEnabled(notebook.ObjectMeta) {
			return admission.Denied(fmt.Sprintf("Cannot have both %s and %s set to true. Pick one.", AnnotationServiceMesh, AnnotationInjectOAuth))
		}
		err = InjectOAuthProxy(notebook, w.OAuthConfig)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}

	// Create the mutated notebook object
	marshaledNotebook, err := json.Marshal(notebook)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledNotebook)
}

// InjectDecoder injects the decoder.
func (w *NotebookWebhook) InjectDecoder(d *admission.Decoder) error {
	w.Decoder = d
	return nil
}

func CheckAndMountCACertBundle(ctx context.Context, cli client.Client, notebook *nbv1.Notebook) error {

	configMapName := "odh-trusted-ca-bundle"

	// get configmap based on its name and the namespace
	configMap := &corev1.ConfigMap{}
	if err := cli.Get(ctx, client.ObjectKey{Namespace: notebook.Namespace, Name: configMapName}, configMap); err != nil {
		return err
	}

	// Search for the odh-trusted-ca-bundle ConfigMap
	fmt.Printf("ConfigMap found on the given Namespace")
	cm := configMap

	// Validate certificates before mounting
	if err := certValidator(cm); err != nil {
		return err
	}

	if cm.Name == configMapName {

		volumeName := "trusted-ca"
		caVolumeMountPath := "/etc/pki/tls/certs/ca-bundle.crt"
		odhVolumeMountPath := "/etc/pki/tls/certs/odh-ca-bundle.crt"
		// Define volume mounts for both certificates
		volumeMounts := []corev1.VolumeMount{
			{
				Name:      volumeName,
				MountPath: caVolumeMountPath,
				SubPath:   "ca-bundle.crt",
				ReadOnly:  true,
			},
			{
				Name:      volumeName,
				MountPath: odhVolumeMountPath,
				SubPath:   "odh-ca-bundle.crt",
				ReadOnly:  true,
			},
		}

		// Add volume mount to the pod's spec
		notebook.Spec.Template.Spec.Containers[0].VolumeMounts = append(notebook.Spec.Template.Spec.Containers[0].VolumeMounts, volumeMounts...)

		// Create volume for mounting the CA certificate from the ConfigMap with key and path
		configMapVolume := corev1.Volume{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name},
					Optional:             pointer.Bool(true),
					Items: []corev1.KeyToPath{
						{
							Key:  "ca-bundle.crt",
							Path: "ca-bundle.crt",
						},
						{
							Key:  "odh-ca-bundle.crt",
							Path: "odh-ca-bundle.crt",
						},
					},
				},
			},
		}

		// Add volume to the pod's spec
		notebook.Spec.Template.Spec.Volumes = append(notebook.Spec.Template.Spec.Volumes, configMapVolume)

		return nil
	}

	// If specified ConfigMap not found
	fmt.Printf("%s ConfigMap not found\n", configMapName)
	return nil
}

func certValidator(cm *corev1.ConfigMap) error {
	caCertData, ok := cm.Data["ca-bundle.crt"]
	if !ok {
		return errors.New("ca-bundle.crt not found in the ConfigMap")
	}

	// Attempt to decode PEM encoded certificate
	caBlock, _ := pem.Decode([]byte(caCertData))
	if caBlock != nil && caBlock.Type == "CERTIFICATE" {
		// Attempt to parse the certificate
		_, err := x509.ParseCertificate(caBlock.Bytes)
		if err != nil {
			return fmt.Errorf("error parsing certificate for key 'ca-bundle.crt' in ConfigMap odh-trusted-ca-bundle: %v", err)
		}
	} else if len(caCertData) > 0 {
		return fmt.Errorf("invalid certificate format for key 'ca-bundle.crt' in ConfigMap odh-trusted-ca-bundle")
	}

	odhCertData, ok := cm.Data["odh-ca-bundle.crt"]
	if !ok || odhCertData == "" {
		// Print a warning if odh-ca-bundle.crt data is empty
		fmt.Println("Warning: odh-ca-bundle.crt data is empty")
		return nil
	}

	// Attempt to decode PEM encoded certificate
	odhBlock, _ := pem.Decode([]byte(odhCertData))
	if odhBlock != nil && odhBlock.Type == "CERTIFICATE" {
		// Attempt to parse the certificate
		_, err := x509.ParseCertificate(odhBlock.Bytes)
		if err != nil {
			return fmt.Errorf("error parsing certificate for key 'odh-ca-bundle.crt' in ConfigMap odh-trusted-ca-bundle: %v", err)
		}
	} else if len(odhCertData) > 0 {
		return fmt.Errorf("invalid certificate format for key 'odh-ca-bundle.crt' in ConfigMap odh-trusted-ca-bundle")
	}

	return nil
}
