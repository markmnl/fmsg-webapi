package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSecurityHeadersSetsNosniff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(SecurityHeaders())
	r.GET("/f", func(c *gin.Context) {
		c.Header("Content-Disposition", "attachment; filename=x.html")
		c.Data(http.StatusOK, "text/html", []byte("<script>alert(1)</script>"))
	})
	r.NoRoute(func(c *gin.Context) { c.Status(http.StatusNotFound) })

	for _, path := range []string{"/f", "/missing"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
	}
}
