package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	webhooksvc "rclone-manager/internal/webhook"
)

func webhookAuthMiddleware(c *gin.Context) {
	if webhookJobs == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "webhook service is not ready"})
		return
	}
	token := bearerToken(c.GetHeader("Authorization"))
	if token == "" {
		token = strings.TrimSpace(c.GetHeader("X-API-Token"))
	}
	if token == "" {
		token = strings.TrimSpace(c.GetHeader("X-Webhook-Token"))
	}
	if token == "" {
		token = strings.TrimSpace(c.Query("token"))
	}
	if !webhookJobs.VerifyToken(token) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.Next()
}

func createWebhookJob(c *gin.Context) {
	if isExternalWebhookRequest(c) {
		webhookAuthMiddleware(c)
		if c.IsAborted() {
			return
		}
	}

	var req webhooksvc.CreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	job, err := webhookJobs.CreateJob(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID, "job_type": job.JobType, "status": job.Status})
}

func isExternalWebhookRequest(c *gin.Context) bool {
	return c.FullPath() == "/webhook"
}

func listWebhookJobs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	jobs, err := webhookJobs.ListJobs(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"jobs": jobs})
}

func getWebhookJob(c *gin.Context) {
	job, err := webhookJobs.GetJob(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, job)
}

func retryWebhookJob(c *gin.Context) {
	job, err := webhookJobs.RetryJob(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID, "job_type": job.JobType, "status": job.Status})
}

func getWebhookConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"local_base_dir":          cfgGlobal.WebhookLocalBaseDir,
		"rclone_path":             cfgGlobal.WebhookRclonePath,
		"rclone_config":           cfgGlobal.RcloneConfig,
		"rclone_remote":           cfgGlobal.WebhookRcloneRemote,
		"transfers":               cfgGlobal.WebhookTransfers,
		"checkers":                cfgGlobal.WebhookCheckers,
		"retries":                 cfgGlobal.WebhookRetries,
		"low_level_retries":       cfgGlobal.WebhookLowLevelRetries,
		"bwlimit":                 cfgGlobal.WebhookBWLimit,
		"workers":                 cfgGlobal.WebhookWorkers,
		"queue_size":              cfgGlobal.WebhookQueueSize,
		"job_timeout":             cfgGlobal.WebhookJobTimeout,
		"http_timeout":            cfgGlobal.WebhookHTTPTimeout,
		"max_rclone_log_bytes":    cfgGlobal.WebhookMaxRcloneLogSize,
		"allowed_callback_hosts":  cfgGlobal.AllowedCallbackHosts,
		"allowed_curl_hosts":      cfgGlobal.AllowedCurlHosts,
		"token_required":          webhookJobs.AuthEnabled(),
		"api_token_enabled":       strings.TrimSpace(cfgGlobal.APIToken) != "",
		"token_source":            "RCLONE_MANAGER_API_TOKEN",
		"allow_anonymous_webhook": cfgGlobal.WebhookAllowAnonymous,
	})
}

func bearerToken(value string) string {
	if value == "" {
		return ""
	}
	prefix := "Bearer "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(value[len(prefix):])
}
