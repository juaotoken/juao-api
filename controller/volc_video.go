package controller

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/gin-gonic/gin"
)

var volcVideoListStatuses = []string{
	string(model.TaskStatusNotStart),
	model.TaskStatusSubmitted,
	model.TaskStatusQueued,
	model.TaskStatusInProgress,
	model.TaskStatusSuccess,
	model.TaskStatusFailure,
}

func volcVideoListError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"error": gin.H{
		"message": common.MessageWithRequestId(message, c.GetString(common.RequestIdKey)),
		"type":    "invalid_request_error",
	}})
}

// RelayVolcVideoList handles GET /v1/video/generations: the caller's
// Volcengine Video (type 500) tasks, paged by page_num / page_size and
// optionally filtered by status.
func RelayVolcVideoList(c *gin.Context) {
	pageNum := 1
	if raw := c.Query("page_num"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			volcVideoListError(c, http.StatusBadRequest, "page_num must be an integer")
			return
		}
		pageNum = max(value, 1)
	}
	pageSize := 20
	if raw := c.Query("page_size"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			volcVideoListError(c, http.StatusBadRequest, "page_size must be an integer")
			return
		}
		pageSize = min(max(value, 1), 100)
	}
	status := c.Query("status")
	if status != "" && !slices.Contains(volcVideoListStatuses, status) {
		volcVideoListError(c, http.StatusBadRequest, "invalid status")
		return
	}
	ascending := false
	switch c.DefaultQuery("order", "desc") {
	case "desc":
	case "asc":
		ascending = true
	default:
		volcVideoListError(c, http.StatusBadRequest, "order must be asc or desc")
		return
	}

	platform := constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeVolcVideo))
	tasks, total, err := model.ListUserTasksByPlatform(c.GetInt("id"), platform, status, ascending, (pageNum-1)*pageSize, pageSize)
	if err != nil {
		logger.LogError(c, "list volcengine video tasks failed: "+err.Error())
		volcVideoListError(c, http.StatusInternalServerError, "database error")
		return
	}
	items := make([]*dto.TaskDto, 0, len(tasks))
	for _, task := range tasks {
		items = append(items, relay.TaskModel2Dto(task))
	}
	c.JSON(http.StatusOK, gin.H{
		"object":    "list",
		"data":      items,
		"total":     total,
		"page_num":  pageNum,
		"page_size": pageSize,
	})
}
