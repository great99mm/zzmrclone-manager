package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"rclone-manager/internal/auth"
)

func TestManualAnalyzeRoutesRequireAdministrator(t *testing.T) {
	previous := cfgGlobal
	cfgGlobal = nil
	defer func() { cfgGlobal = previous }()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/tasks/1/manual-runs/analyze", requireAdminStrictTokenOrSession, func(c *gin.Context) { c.Status(http.StatusNoContent) })

	cases := []struct {
		name string
		auth string
		want int
	}{
		{name: "anonymous", want: http.StatusForbidden},
		{name: "regular session", auth: "Bearer " + auth.IssueToken("operator", false), want: http.StatusForbidden},
		{name: "admin session", auth: "Bearer " + auth.IssueToken("admin", true), want: http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/tasks/1/manual-runs/analyze", nil)
			if tc.auth != "" {
				request.Header.Set("Authorization", tc.auth)
			}
			router.ServeHTTP(recorder, request)
			if recorder.Code != tc.want {
				t.Fatalf("status=%d, want %d; body=%s", recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}

func TestManualWorkerRoutesRequireAdministrator(t *testing.T) {
	previous := cfgGlobal
	cfgGlobal = nil
	defer func() { cfgGlobal = previous }()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := func(c *gin.Context) { c.Status(http.StatusNoContent) }
	router.POST("/api/manual-runs/1/start", requireAdminStrictTokenOrSession, handler)
	router.GET("/api/manual-runs/1/workers", requireAdminStrictTokenOrSession, handler)
	router.GET("/api/manual-workers/1", requireAdminStrictTokenOrSession, handler)
	router.POST("/api/manual-workers/1/cancel", requireAdminStrictTokenOrSession, handler)
	router.POST("/api/manual-workers/1/retry", requireAdminStrictTokenOrSession, handler)
	router.GET("/api/manual-workers/1/logs", requireAdminStrictTokenOrSession, handler)

	cases := []struct {
		name   string
		method string
		path   string
		auth   string
		want   int
	}{
		{name: "anonymous start", method: http.MethodPost, path: "/api/manual-runs/1/start", want: http.StatusForbidden},
		{name: "regular worker", method: http.MethodGet, path: "/api/manual-workers/1", auth: "Bearer " + auth.IssueToken("operator", false), want: http.StatusForbidden},
		{name: "admin logs", method: http.MethodGet, path: "/api/manual-workers/1/logs", auth: "Bearer " + auth.IssueToken("admin", true), want: http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.auth != "" {
				request.Header.Set("Authorization", tc.auth)
			}
			router.ServeHTTP(recorder, request)
			if recorder.Code != tc.want {
				t.Fatalf("status=%d, want %d; body=%s", recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}
