package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"rclone-manager/internal/models"
)

func applyMountStatus(cfg *models.MountConfig) {
	if cfg == nil || mountMgr == nil {
		return
	}
	mountMgr.ApplyRuntimeStatus(cfg)
}

func getMountSystemInfo(c *gin.Context) {
	if mountMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "mount manager not ready"})
		return
	}
	c.JSON(http.StatusOK, mountMgr.SupportInfo())
}

func listMounts(c *gin.Context) {
	configs := make([]models.MountConfig, 0)
	db.Order("created_at desc").Find(&configs)
	for i := range configs {
		applyMountStatus(&configs[i])
	}
	c.JSON(http.StatusOK, configs)
}

func getMount(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var cfg models.MountConfig
	if err := db.First(&cfg, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "mount not found"})
		return
	}
	applyMountStatus(&cfg)
	c.JSON(http.StatusOK, cfg)
}

func createMount(c *gin.Context) {
	if mountMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "mount manager not ready"})
		return
	}

	var cfg models.MountConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := mountMgr.NormalizeAndValidate(&cfg, 0); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfg.Status = "stopped"
	if err := db.Create(&cfg).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	applyMountStatus(&cfg)
	c.JSON(http.StatusCreated, cfg)
}

func updateMount(c *gin.Context) {
	if mountMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "mount manager not ready"})
		return
	}

	id, _ := strconv.Atoi(c.Param("id"))
	var current models.MountConfig
	if err := db.First(&current, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "mount not found"})
		return
	}
	applyMountStatus(&current)

	var updates models.MountConfig
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := mountMgr.NormalizeAndValidate(&updates, uint(id)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := db.Model(&current).Updates(map[string]interface{}{
		"name":           updates.Name,
		"remote_name":    updates.RemoteName,
		"remote_path":    updates.RemotePath,
		"mount_path":     current.MountPath,
		"rclone_config":  updates.RcloneConfig,
		"enabled":        updates.Enabled,
		"allow_other":    updates.AllowOther,
		"read_only":      updates.ReadOnly,
		"vfs_cache_mode": updates.VFSCacheMode,
		"dir_cache_time": updates.DirCacheTime,
		"poll_interval":  updates.PollInterval,
		"uid":            updates.UID,
		"gid":            updates.GID,
		"extra_args":     updates.ExtraArgs,
		"status":         current.Status,
		"last_error":     "",
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	db.First(&current, id)
	applyMountStatus(&current)
	c.JSON(http.StatusOK, current)
}

func deleteMount(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var cfg models.MountConfig
	if err := db.First(&cfg, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "mount not found"})
		return
	}
	applyMountStatus(&cfg)
	if mountMgr != nil {
		if cfg.Status == "mounted" || cfg.Status == "starting" || cfg.Status == "stopping" {
			if err := mountMgr.Stop(uint(id)); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
	}
	if err := db.Delete(&models.MountConfig{}, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "mount deleted"})
}

func startMount(c *gin.Context) {
	if mountMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "mount manager not ready"})
		return
	}
	id, _ := strconv.Atoi(c.Param("id"))
	var cfg models.MountConfig
	if err := db.First(&cfg, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "mount not found"})
		return
	}
	if err := mountMgr.Start(&cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	db.First(&cfg, id)
	applyMountStatus(&cfg)
	c.JSON(http.StatusOK, cfg)
}

func stopMount(c *gin.Context) {
	if mountMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "mount manager not ready"})
		return
	}
	id, _ := strconv.Atoi(c.Param("id"))
	var cfg models.MountConfig
	if err := db.First(&cfg, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "mount not found"})
		return
	}
	if err := mountMgr.Stop(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	db.First(&cfg, id)
	applyMountStatus(&cfg)
	c.JSON(http.StatusOK, cfg)
}
