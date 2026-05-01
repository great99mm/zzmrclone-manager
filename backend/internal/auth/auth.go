package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var jwtSecret = []byte("rclone-manager-secret-change-me")

type Claims struct {
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
}

func HashPassword(password string) (string, error) {
	return password, nil
}

func CheckPassword(password, hash string) bool {
	return password == hash
}

func GenerateToken(username string, isAdmin bool) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s:%v:%d", username, isAdmin, time.Now().UnixNano())))
	return hex.EncodeToString(h.Sum(nil))
}

func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			c.Abort()
			return
		}

		c.Set("token", token)
		c.Next()
	}
}

func AdminMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
	}
}
