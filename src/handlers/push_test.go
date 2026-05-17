package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/markmnl/fmsg-webapi/middleware"
)

func TestShouldPrune(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{http.StatusOK, false},
		{http.StatusCreated, false},
		{http.StatusNotFound, true},
		{http.StatusGone, true},
		{http.StatusInternalServerError, false},
		{http.StatusTooManyRequests, false},
	}
	for _, tt := range tests {
		if got := shouldPrune(tt.status); got != tt.want {
			t.Errorf("shouldPrune(%d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestBuildPushPayload(t *testing.T) {
	item := &messageListItem{From: "@bob@example.com", ShortText: "hello there"}
	got := buildPushPayload(item, 7, "/icon-192.png")

	want := pushPayload{
		Title:    "@bob@example.com",
		Body:     "hello there",
		ThreadID: 7,
		URL:      "/app3.html?thread=7",
		Tag:      "thread-7",
		Icon:     "/icon-192.png",
	}
	if got != want {
		t.Errorf("buildPushPayload = %+v, want %+v", got, want)
	}
}

// runPushHandler invokes a handler with the given JSON body and an
// authenticated identity, returning the response recorder.
func runPushHandler(handler gin.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/fmsg/push/subscribe", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(middleware.IdentityKey, "@alice@example.com")
	handler(c)
	return w
}

func TestPushSubscribe_RejectsInvalidBody(t *testing.T) {
	h := &PushHandler{}
	tests := []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"missing endpoint", `{"keys":{"p256dh":"p","auth":"a"}}`},
		{"missing p256dh", `{"endpoint":"https://push/x","keys":{"auth":"a"}}`},
		{"missing auth", `{"endpoint":"https://push/x","keys":{"p256dh":"p"}}`},
		{"empty keys", `{"endpoint":"https://push/x","keys":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := runPushHandler(h.Subscribe, http.MethodPost, tt.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestPushUnsubscribe_RejectsInvalidBody(t *testing.T) {
	h := &PushHandler{}
	tests := []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"missing endpoint", `{}`},
		{"empty endpoint", `{"endpoint":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := runPushHandler(h.Unsubscribe, http.MethodDelete, tt.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}
