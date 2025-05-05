package controllers

import (
	"context"
	"fmt"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("When Creating a notebook it should mount the Elyra runtime secret properly", func() {
	ctx := context.Background()

	const (
		Namespace = "default"
	)

	BeforeEach(func() {
		err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace}}, &client.CreateOptions{})
		if err != nil && !apierrs.IsAlreadyExists(err) {
			Expect(err).ToNot(HaveOccurred())
		}
	})

	testCases := []struct {
		name              string
		notebookName      string
		secretData        map[string][]byte
		expectedMountName string
		expectedMountPath string
	}{
		{
			name:         "Pipeline secret with data",
			notebookName: "test-notebook-1",
			secretData: map[string][]byte{
				"odh_dsp.json": []byte(`{"display_name":"Data Science Pipeline"}`),
			},
			expectedMountName: "elyra-dsp-details",
			expectedMountPath: "/opt/app-root/runtimes",
		},
		{
			name:              "Pipeline secret without data",
			notebookName:      "test-notebook-2",
			secretData:        map[string][]byte{},
			expectedMountName: "",
			expectedMountPath: "",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		It(fmt.Sprintf("should mount secret correctly: %s", testCase.name), func() {
			notebook := createNotebook(testCase.notebookName, Namespace)
			secretName := "ds-pipeline-config-" + testCase.notebookName

			// Clean up previous resources
			_ = cli.Delete(ctx, notebook)
			_ = cli.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: Namespace}})

			By("Waiting for the Notebook to be deleted")
			Eventually(func(g Gomega) {
				err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, &nbv1.Notebook{})
				g.Expect(apierrs.IsNotFound(err)).To(BeTrue())
			}).WithOffset(1).Should(Succeed())

			By("Creating the Secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: Namespace,
				},
				Data: testCase.secretData,
				Type: corev1.SecretTypeOpaque,
			}
			Expect(cli.Create(ctx, secret)).To(Succeed())

			By("Creating the Notebook")
			Expect(cli.Create(ctx, notebook)).To(Succeed())

			By("Fetching the created Notebook")
			typedNotebook := &nbv1.Notebook{}
			Eventually(func(g Gomega) {
				err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, typedNotebook)
				g.Expect(err).ToNot(HaveOccurred())
			}).Should(Succeed())

			// Check VolumeMounts
			container := typedNotebook.Spec.Template.Spec.Containers[0]
			foundMount := false
			for _, vm := range container.VolumeMounts {
				if vm.Name == testCase.expectedMountName && vm.MountPath == testCase.expectedMountPath {
					foundMount = true
					break
				}
			}
			if testCase.expectedMountName != "" {
				Expect(foundMount).To(BeTrue(), "expected VolumeMount not found")
			} else {
				Expect(foundMount).To(BeFalse(), "unexpected VolumeMount found")
			}

			// Check Volumes
			foundVolume := false
			for _, v := range typedNotebook.Spec.Template.Spec.Volumes {
				if v.Name == testCase.expectedMountName && v.Secret != nil && v.Secret.SecretName == secretName {
					foundVolume = true
					break
				}
			}
			if testCase.expectedMountName != "" {
				Expect(foundVolume).To(BeTrue(), "expected Secret volume not found")
			} else {
				Expect(foundVolume).To(BeFalse(), "unexpected Secret volume found")
			}

			By("Cleaning up resources")
			Expect(cli.Delete(ctx, notebook)).To(Succeed())
			Expect(cli.Delete(ctx, secret)).To(Succeed())
		})
	}
})
