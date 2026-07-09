package handlers

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestActAsParam(t *testing.T) {
	tests := []struct {
		name   string
		header string
		query  string
		want   string
	}{
		{name: "neither set", want: ""},
		{name: "header only", header: "@bot@example.com", want: "@bot@example.com"},
		{name: "query only", query: "@bot@example.com", want: "@bot@example.com"},
		{name: "header wins over query", header: "@a@example.com", query: "@b@example.com", want: "@a@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			target := "/fmsg/ws"
			if tt.query != "" {
				target += "?act_as=" + url.QueryEscape(tt.query)
			}
			c.Request = httptest.NewRequest("GET", target, nil)
			if tt.header != "" {
				c.Request.Header.Set("X-FMSG-Act-As", tt.header)
			}
			if got := actAsParam(c); got != tt.want {
				t.Errorf("actAsParam() = %q, want %q", got, tt.want)
			}
		})
	}
}
