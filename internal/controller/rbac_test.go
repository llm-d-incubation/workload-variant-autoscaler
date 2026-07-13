package controller_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRBACMarkersLeastPrivilege verifies kubebuilder:rbac markers follow least-privilege.
func TestRBACMarkersLeastPrivilege(t *testing.T) {
	rbacFile := "rbac.go"
	content, err := os.ReadFile(rbacFile)
	require.NoError(t, err, "Failed to read rbac.go")

	rbacContent := string(content)

	// Nodes should only have read verbs (get;list;watch), not write (update;patch)
	t.Run("nodes permissions are read-only", func(t *testing.T) {
		assert.NotContains(t, rbacContent, `resources=nodes,verbs=get;list;watch;update;patch`,
			"nodes should not have update;patch verbs (controller only lists nodes for GPU discovery)")
		assert.Contains(t, rbacContent, `resources=nodes,verbs=get;list;watch`,
			"nodes should have read-only verbs")
	})

	// nodes/status should only have read verbs
	t.Run("nodes/status permissions are read-only", func(t *testing.T) {
		assert.NotContains(t, rbacContent, `resources=nodes/status,verbs=get;list;update;patch;watch`,
			"nodes/status should not have update;patch verbs (unused write permissions)")
		assert.Contains(t, rbacContent, `resources=nodes/status,verbs=get;list;watch`,
			"nodes/status should have read-only verbs")
	})

	// ConfigMaps cluster-wide should only have read verbs (reconciler is read-only)
	t.Run("configmaps permissions are read-only at cluster scope", func(t *testing.T) {
		lines := strings.Split(rbacContent, "\n")
		for i, line := range lines {
			if strings.Contains(line, `resources=configmaps`) &&
				!strings.Contains(line, "configmaps/status") &&
				strings.Contains(line, "rbac.go") {
				context := strings.Join(lines[max(0, i-2):min(len(lines), i+3)], "\n")
				assert.NotContains(t, context, "update",
					"configmaps should not have update verb in cluster-wide RBAC marker")
			}
		}
	})
}

// TestSecretRBACIsNamespaced verifies cluster-wide Secret permissions are replaced
// with a namespaced Role for epp-metrics-token.
func TestSecretRBACIsNamespaced(t *testing.T) {
	rbacDir := filepath.Join("..", "..", "config", "base", "rbac")

	t.Run("cluster-wide secret permissions removed from ClusterRole", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(rbacDir, "manager-clusterrole.yaml"))
		require.NoError(t, err)

		assert.NotContains(t, string(content), "- secrets",
			"Secrets should not have cluster-wide permissions in ClusterRole")
	})

	t.Run("namespaced Role grants get on epp-metrics-token", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(rbacDir, "epp-metrics-role.yaml"))
		require.NoError(t, err)

		role := string(content)
		assert.Contains(t, role, "kind: Role", "Should be a namespaced Role, not ClusterRole")
		assert.Contains(t, role, "- secrets", "Should grant permissions on secrets")
		assert.NotContains(t, role, "resourceNames",
			"Should not use resourceNames (incompatible with list/watch verbs)")
		assert.Contains(t, role, "- get", "Should grant get verb")
		assert.Contains(t, role, "- list", "Should grant list verb (required by informer cache)")
		assert.Contains(t, role, "- watch", "Should grant watch verb (required by informer cache)")
	})

	t.Run("RoleBinding binds manager ServiceAccount to Role", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(rbacDir, "epp-metrics-rolebinding.yaml"))
		require.NoError(t, err)

		binding := string(content)
		assert.Contains(t, binding, "kind: RoleBinding", "Should be a RoleBinding")
		assert.Contains(t, binding, "name: controller-manager", "Should bind controller-manager ServiceAccount")
		assert.Contains(t, binding, "name: epp-metrics-role", "Should reference epp-metrics-role")
	})

	t.Run("cluster-wide secret marker removed from rbac.go", func(t *testing.T) {
		content, err := os.ReadFile("rbac.go")
		require.NoError(t, err)

		rbacContent := string(content)
		assert.NotContains(t, rbacContent, `resources=secrets`,
			"rbac.go should not have a kubebuilder:rbac marker for secrets (moved to namespaced Role)")
		assert.Contains(t, rbacContent, "epp-metrics-role.yaml",
			"rbac.go should reference the namespaced Role file")
	})
}
