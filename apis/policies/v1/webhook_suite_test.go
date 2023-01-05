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

package v1

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	//+kubebuilder:scaffold:imports
	"github.com/kubewarden/kubewarden-controller/internal/pkg/constants"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/envtest/printer"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var k8sClient client.Client
var testEnv *envtest.Environment
var ctx context.Context
var cancel context.CancelFunc

func TestWebhooks(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecsWithDefaultAndCustomReporters(t,
		"Webhook Suite",
		[]Reporter{printer.NewlineReporter{}})
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: false,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "..", "config", "webhook")},
		},
	}

	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	scheme := runtime.NewScheme()
	err = AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = admissionv1beta1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	//+kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// start webhook server using Manager
	webhookInstallOptions := &testEnv.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:             scheme,
		Host:               webhookInstallOptions.LocalServingHost,
		Port:               webhookInstallOptions.LocalServingPort,
		CertDir:            webhookInstallOptions.LocalServingCertDir,
		LeaderElection:     false,
		MetricsBindAddress: "0",
	})
	Expect(err).NotTo(HaveOccurred())

	err = (&ClusterAdmissionPolicy{}).SetupWebhookWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&PolicyServer{}).SetupWebhookWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	//+kubebuilder:scaffold:webhook

	go func() {
		err = mgr.Start(ctx)
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
	}()

	// wait for the webhook server to get ready
	dialer := &net.Dialer{Timeout: time.Second}
	addrPort := fmt.Sprintf("%s:%d", webhookInstallOptions.LocalServingHost, webhookInstallOptions.LocalServingPort)
	Eventually(func() error {
		//nolint:gosec
		conn, err := tls.DialWithDialer(dialer, "tcp", addrPort, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return fmt.Errorf("failed polling webhook server: %w", err)
		}
		conn.Close()
		return nil
	}).Should(Succeed())

}, 60)

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

func makeClusterAdmissionPolicyTemplate(name, namespace, policyServerName string, withRules bool) *ClusterAdmissionPolicy {
	rules := make([]admissionregistrationv1.RuleWithOperations, 0)

	if withRules {
		rules = append(rules, admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{"*"},
				APIVersions: []string{"*"},
				Resources:   []string{"*/*"},
			},
		})
	} else {
		rules = append(rules, admissionregistrationv1.RuleWithOperations{})
	}

	return &ClusterAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: ClusterAdmissionPolicySpec{
			PolicySpec: PolicySpec{
				PolicyServer: policyServerName,
				Settings: runtime.RawExtension{
					Raw: []byte("{}"),
				},
				Rules: rules,
			},
		},
	}
}

//nolint:dupl
func deleteClusterAdmissionPolicy(ctx context.Context, name, namespace string) {
	nsn := types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}
	pol := &ClusterAdmissionPolicy{}
	err := k8sClient.Get(ctx, nsn, pol)
	if apierrors.IsNotFound(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred())

	Expect(k8sClient.Delete(ctx, pol)).To(Succeed())

	// Remove finalizer
	err = k8sClient.Get(ctx, nsn, pol)
	Expect(err).NotTo(HaveOccurred())
	polUpdated := pol.DeepCopy()
	controllerutil.RemoveFinalizer(polUpdated, constants.KubewardenFinalizer)
	err = k8sClient.Update(ctx, polUpdated)
	if err != nil {
		fmt.Fprint(GinkgoWriter, err)
	}
	Expect(err).NotTo(HaveOccurred())

	Eventually(func() bool {
		err := k8sClient.Get(ctx, nsn, &ClusterAdmissionPolicy{})
		return apierrors.IsNotFound(err)
	}).Should(BeTrue())
}

func makePolicyServerTemplate(name, namespace string) *PolicyServer {
	return &PolicyServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: PolicyServerSpec{
			Image:    "image",
			Replicas: 1,
		},
	}
}

//nolint:dupl
func deletePolicyServer(ctx context.Context, name, namespace string) {
	nsn := types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}
	pol := &PolicyServer{}
	err := k8sClient.Get(ctx, nsn, pol)
	if apierrors.IsNotFound(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred())

	Expect(k8sClient.Delete(ctx, pol)).To(Succeed())

	// Remove finalizer
	err = k8sClient.Get(ctx, nsn, pol)
	Expect(err).NotTo(HaveOccurred())
	polUpdated := pol.DeepCopy()
	controllerutil.RemoveFinalizer(polUpdated, constants.KubewardenFinalizer)
	err = k8sClient.Update(ctx, polUpdated)
	if err != nil {
		fmt.Fprint(GinkgoWriter, err)
	}
	Expect(err).NotTo(HaveOccurred())

	Eventually(func() bool {
		err := k8sClient.Get(ctx, nsn, &ClusterAdmissionPolicy{})
		return apierrors.IsNotFound(err)
	}).Should(BeTrue())
}

var _ = Describe("validate ClusterAdmissionPolicy webhook with ", func() {
	namespace := "default"

	It("should accept creating ClusterAdmissionPolicy", func() {
		pol := makeClusterAdmissionPolicyTemplate("policy-test", namespace, "policy-server-foo", true)
		Expect(k8sClient.Create(ctx, pol)).To(Succeed())
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pol), pol)
		if err != nil {
			fmt.Fprint(GinkgoWriter, err)
		}
		Expect(err).NotTo(HaveOccurred())

		By("checking default values")
		// Testing for PolicyStatus == "unscheduled" can't happen here, Status
		// subresources can't be defaulted
		Expect(pol.ObjectMeta.Finalizers).To(HaveLen(1))
		Expect(pol.ObjectMeta.Finalizers[0]).To(Equal(constants.KubewardenFinalizer))
		Expect(pol.Spec.PolicyServer).To(Equal("policy-server-foo"))

		By("deleting the created ClusterAdmissionPolicy")
		deleteClusterAdmissionPolicy(ctx, "policy-test", namespace)
	})

	It("should deny updating ClusterAdmissionPolicy if policyServer name is changed", func() {
		pol := makeClusterAdmissionPolicyTemplate("policy-test2", namespace, "policy-server-bar", true)
		Expect(k8sClient.Create(ctx, pol)).To(Succeed())

		pol.Spec.PolicyServer = "policy-server-changed"
		Expect(k8sClient.Update(ctx, pol)).NotTo(Succeed())

		By("deleting the created ClusterAdmissionPolicy")
		deleteClusterAdmissionPolicy(ctx, "policy-test2", namespace)
	})

	It("should fail to create a ClusterAdmissionPolicy with only empty rules", func() {
		pol := makeClusterAdmissionPolicyTemplate("policy-test", namespace, "policy-server-foo", false)
		err := k8sClient.Create(ctx, pol)
		Expect(err).To(HaveOccurred())
	})

	It("should fail to update to a ClusterAdmissionPolicy with only empty rules", func() {
		pol := makeClusterAdmissionPolicyTemplate("policy-test", namespace, "policy-server-foo", true)
		Expect(k8sClient.Create(ctx, pol)).To(Succeed())

		pol.Spec.Rules = []admissionregistrationv1.RuleWithOperations{{}}
		err := k8sClient.Update(ctx, pol)
		Expect(err).To(HaveOccurred())

		By("deleting the created ClusterAdmissionPolicy")
		deleteClusterAdmissionPolicy(ctx, "policy-test", namespace)
	})
})

var _ = Describe("validate PolicyServer webhook with ", func() {
	namespace := "default"

	It("should add kubewarden finalizer when creating a PolicyServer", func() {
		pol := makePolicyServerTemplate("policyserver-test", namespace)
		Expect(k8sClient.Create(ctx, pol)).To(Succeed())
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pol), pol)
		if err != nil {
			fmt.Fprint(GinkgoWriter, err)
		}
		Expect(err).NotTo(HaveOccurred())

		By("checking default values")
		Expect(pol.ObjectMeta.Finalizers).To(HaveLen(1))
		Expect(pol.ObjectMeta.Finalizers[0]).To(Equal(constants.KubewardenFinalizer))

		By("deleting the created PolicyServer")
		deletePolicyServer(ctx, "policyserver-test", namespace)
	})

})
