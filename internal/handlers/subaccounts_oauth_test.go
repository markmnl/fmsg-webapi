package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// Sub-account administration belongs to owner sessions only. This is the
// handler-level guard behind the middleware's delegated-token route table.
func TestRequireIdPOwnerByAuthType(t *testing.T) {
	const owner = "@alice@example.com"
	tests := []struct {
		authType, identity string
		want               bool
	}{
		{middleware.AuthTypeIdP, owner, true},
		{middleware.AuthTypeIdP, "@alice_bot@example.com", false},
		{middleware.AuthTypeOAuth, owner, false},
		{middleware.AuthTypeAPI, owner, false},
		{"", owner, false},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(middleware.AuthTypeKey, tt.authType)
		c.Set(middleware.IdentityKey, tt.identity)
		c.Set(middleware.OwnerIdentityKey, owner)
		_, ok := requireIdPOwner(c)
		if ok != tt.want {
			t.Errorf("auth_type=%q identity=%s: ok=%v want %v", tt.authType, tt.identity, ok, tt.want)
		}
		if !ok && w.Code != http.StatusForbidden {
			t.Errorf("auth_type=%q: status=%d want 403", tt.authType, w.Code)
		}
	}
}
