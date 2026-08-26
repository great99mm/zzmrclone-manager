package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"rclone-manager/internal/auth"
	"rclone-manager/internal/config"
)

func TestProtectedAPIRoutesRequireLiveSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousConfig := cfgGlobal
	cfgGlobal = &config.Config{}
	t.Cleanup(func() { cfgGlobal = previousConfig })

	router := gin.New()
	api := router.Group("/api")
	api.Use(requireStrictTokenOrSession)
	api.POST("/tasks", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	tests := []struct {
		name          string
		authorization string
		want          int
	}{
		{name: "missing session", want: http.StatusUnauthorized},
		{name: "unknown session", authorization: "Bearer expired-session", want: http.StatusUnauthorized},
		{name: "live session", authorization: "Bearer " + auth.IssueToken("admin", true), want: http.StatusNoContent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
		})
	}
}
