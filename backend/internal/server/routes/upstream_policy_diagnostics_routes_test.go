package routes

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Invalid input must reach the real validation handler for a full admin, but
// the existing least-privilege guard must reject readonly admins first.
func TestUpstreamPolicyDiagnosticsRouteRoleBoundary(t *testing.T) {
	for _, tc := range []struct {
		role string
		want int
	}{
		{service.RoleAdmin, http.StatusBadRequest},
		{service.RoleReadonlyAdmin, http.StatusForbidden},
	} {
		t.Run(tc.role, func(t *testing.T) {
			router := newAdminRouterWithRole(tc.role)
			require.Equal(t, tc.want, statusFor(t, router, http.MethodGet, "/api/v1/admin/accounts/invalid/upstream-policy"))
		})
	}
}
