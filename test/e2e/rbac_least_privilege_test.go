package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("RBAC Least-Privilege Security", Label("full"), func() {

	It("ClusterRole should not grant cluster-wide Secret permissions", func() {
		clusterRole, err := k8sClient.RbacV1().ClusterRoles().Get(ctx, "wva-manager-role", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		for _, rule := range clusterRole.Rules {
			for _, resource := range rule.Resources {
				Expect(resource).NotTo(Equal("secrets"),
					"ClusterRole should not grant cluster-wide Secret permissions")
			}
		}
	})

	It("namespaced Role should grant get on specific secret", func() {
		role, err := k8sClient.RbacV1().Roles(cfg.WVANamespace).Get(ctx, "wva-epp-metrics-role", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(role.Rules).To(HaveLen(1), "Role should have exactly one rule")
		Expect(role.Rules[0].Resources).To(ContainElement("secrets"))
		Expect(role.Rules[0].ResourceNames).To(ConsistOf("wva-epp-metrics-token"),
			"Role should scope access to the specific secret")
		Expect(role.Rules[0].Verbs).To(ConsistOf("get"),
			"Role should only grant get (apiReader bypasses informer cache)")
	})

	It("RoleBinding should bind controller-manager ServiceAccount to epp-metrics Role", func() {
		roleBinding, err := k8sClient.RbacV1().RoleBindings(cfg.WVANamespace).Get(ctx, "wva-epp-metrics-rolebinding", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(roleBinding.RoleRef.Name).To(Equal("wva-epp-metrics-role"))
		Expect(roleBinding.RoleRef.Kind).To(Equal("Role"))
		Expect(roleBinding.Subjects).To(HaveLen(1))
		Expect(roleBinding.Subjects[0].Kind).To(Equal("ServiceAccount"))
		Expect(roleBinding.Subjects[0].Name).To(Equal("wva-controller-manager"))
	})
})
