package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"rclone-manager/internal/auth"
	"rclone-manager/internal/config"
)

func runStrictAuth(t *testing.T, cfg *config.Config, query, authorization string) int {
	t.Helper()
	previous := cfgGlobal
	cfgGlobal = cfg
	defer func() { cfgGlobal = previous }()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/resolve", requireStrictTokenOrSession, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/resolve?"+query, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	router.ServeHTTP(recorder, request)
	return recorder.Code
}

func TestStrictMoveResolutionAuthRequiresSessionOrExactToken(t *testing.T) {
	if code := runStrictAuth(t, &config.Config{}, "", ""); code != http.StatusForbidden {
		t.Fatalf("anonymous optional-token request = %d", code)
	}
	if code := runStrictAuth(t, &config.Config{}, "", "Bearer arbitrary"); code != http.StatusForbidden {
		t.Fatalf("bad bearer optional-token request = %d", code)
	}
	session := auth.IssueToken("phase3", true)
	if code := runStrictAuth(t, &config.Config{}, "", "Bearer "+session); code != http.StatusNoContent {
		t.Fatalf("valid session request = %d", code)
	}
	if code := runStrictAuth(t, &config.Config{APIToken: "configured"}, "token=configured", ""); code != http.StatusNoContent {
		t.Fatalf("exact configured token request = %d", code)
	}
	if code := runStrictAuth(t, &config.Config{APIToken: "configured"}, "token=wrong", "Bearer arbitrary"); code != http.StatusForbidden {
		t.Fatalf("bad configured credentials request = %d", code)
	}
}
