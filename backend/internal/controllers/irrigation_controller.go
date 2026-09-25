package controllers

import (
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"irrigation/internal/models"
	"irrigation/internal/services"
	"irrigation/pkg/response"
)

type IrrigationController struct {
	irrigationService *services.IrrigationService
	budgetService     *services.BudgetService
}

func NewIrrigationController() *IrrigationController {
	return &IrrigationController{
		irrigationService: services.NewIrrigationService(),
		budgetService:     services.NewBudgetService(),
	}
}

// CheckIrrigation godoc
// @Summary 灌溉前检查
// @Description 在计划或手动灌溉启动前，检查当天累计用水量与本次估算量。超出每日限额或同区域已有进行中任务时返回 409（含 reason 和 remaining）；未设置限额的区域始终放行
// @Tags 灌溉执行
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param request body object true "检查参数，如 {\"zone_id\": 1, \"duration\": 600} 或 {\"zone_id\": 1, \"estimated_usage\": 60}"
// @Success 200 {object} services.CheckResult
// @Failure 409 {object} response.Response
// @Router /api/irrigation/check [post]
func (c *IrrigationController) CheckIrrigation(ctx *gin.Context) {
	var req struct {
		ZoneID         uint     `json:"zone_id" binding:"required"`
		Duration       *int     `json:"duration"`
		EstimatedUsage *float64 `json:"estimated_usage"`
	}

	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	estimated := 0.0
	if req.EstimatedUsage != nil {
		estimated = *req.EstimatedUsage
	} else if req.Duration != nil {
		estimated = services.EstimateWaterUsage(*req.Duration)
	}

	result, err := c.budgetService.CheckIrrigation(req.ZoneID, estimated)
	if err != nil {
		var conflict *services.ConflictError
		if errors.As(err, &conflict) {
			response.Conflict(ctx, conflict.Error(), gin.H{
				"reason":      conflict.Reason,
				"remaining":   conflict.Remaining,
				"used_today":  result.UsedToday,
				"estimated":   estimated,
				"in_progress": result.InProgress,
			})
			return
		}
		if errors.Is(err, services.ErrZoneNotFound) {
			response.NotFound(ctx, "Zone not found")
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, result)
}

// ManualIrrigate godoc
// @Summary 手动灌溉
// @Description 触发手动灌溉，启动前检查每日水量限额及同区域进行中任务
// @Tags 灌溉执行
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param request body object true "手动灌溉请求，如 {\"zone_id\": 1, \"duration\": 600}"
// @Success 200 {object} models.IrrigationLog
// @Failure 409 {object} response.Response
// @Router /api/irrigation/manual [post]
func (c *IrrigationController) ManualIrrigate(ctx *gin.Context) {
	var req struct {
		ZoneID         uint     `json:"zone_id" binding:"required"`
		Duration       *int     `json:"duration"`
		EstimatedUsage *float64 `json:"estimated_usage"`
	}

	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	opts := &services.StartIrrigationOptions{}
	if req.EstimatedUsage != nil {
		opts.EstimatedUsage = *req.EstimatedUsage
	}
	if req.Duration != nil {
		opts.DurationSeconds = *req.Duration
	}

	log, err := c.irrigationService.StartIrrigation(nil, &req.ZoneID, models.TriggerTypeManual, opts)
	if err != nil {
		var conflict *services.ConflictError
		if errors.As(err, &conflict) {
			response.Conflict(ctx, conflict.Error(), gin.H{
				"reason":    conflict.Reason,
				"remaining": conflict.Remaining,
			})
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}
	response.Success(ctx, log)
}

// GetIrrigationHistory godoc
// @Summary 获取灌溉历史
// @Description 获取灌溉执行历史记录
// @Tags 灌溉执行
// @Security ApiKeyAuth
// @Produce json
// @Param zone_id query int false "区域ID"
// @Param start_time query string false "开始时间 (RFC3339)"
// @Param end_time query string false "结束时间 (RFC3339)"
// @Param limit query int false "返回数量限制" default(100)
// @Success 200 {array} models.IrrigationLog
// @Router /api/irrigation/history [get]
func (c *IrrigationController) GetHistory(ctx *gin.Context) {
	var zoneID *uint
	if zoneIDStr := ctx.Query("zone_id"); zoneIDStr != "" {
		id, _ := strconv.ParseUint(zoneIDStr, 10, 32)
		idUint := uint(id)
		zoneID = &idUint
	}

	var startTime, endTime time.Time
	if startStr := ctx.Query("start_time"); startStr != "" {
		startTime, _ = time.Parse(time.RFC3339, startStr)
	}
	if endStr := ctx.Query("end_time"); endStr != "" {
		endTime, _ = time.Parse(time.RFC3339, endStr)
	}

	limit := 100
	if limitStr := ctx.Query("limit"); limitStr != "" {
		limit, _ = strconv.Atoi(limitStr)
	}

	logs, err := c.irrigationService.GetIrrigationHistory(zoneID, startTime, endTime, limit)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, logs)
}

// GetWaterUsageStats godoc
// @Summary 获取用水量统计
// @Description 获取指定时间段的用水量统计
// @Tags 用水统计
// @Security ApiKeyAuth
// @Produce json
// @Param zone_id query int false "区域ID"
// @Param start_time query string false "开始时间 (RFC3339)"
// @Param end_time query string false "结束时间 (RFC3339)"
// @Success 200 {object} services.WaterUsageStats
// @Router /api/statistics/water-usage [get]
func (c *IrrigationController) GetWaterUsageStats(ctx *gin.Context) {
	var zoneID *uint
	if zoneIDStr := ctx.Query("zone_id"); zoneIDStr != "" {
		id, _ := strconv.ParseUint(zoneIDStr, 10, 32)
		idUint := uint(id)
		zoneID = &idUint
	}

	var startTime, endTime time.Time
	if startStr := ctx.Query("start_time"); startStr != "" {
		startTime, _ = time.Parse(time.RFC3339, startStr)
	}
	if endStr := ctx.Query("end_time"); endStr != "" {
		endTime, _ = time.Parse(time.RFC3339, endStr)
	}

	stats, err := c.irrigationService.GetWaterUsageStats(zoneID, startTime, endTime)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, stats)
}

// GetZoneWaterUsage godoc
// @Summary 获取各区域用水量
// @Description 获取各区域的用水量分布
// @Tags 用水统计
// @Security ApiKeyAuth
// @Produce json
// @Param start_time query string false "开始时间 (RFC3339)"
// @Param end_time query string false "结束时间 (RFC3339)"
// @Success 200 {array} services.ZoneWaterUsage
// @Router /api/statistics/zone-usage [get]
func (c *IrrigationController) GetZoneWaterUsage(ctx *gin.Context) {
	var startTime, endTime time.Time
	if startStr := ctx.Query("start_time"); startStr != "" {
		startTime, _ = time.Parse(time.RFC3339, startStr)
	} else {
		startTime = time.Now().AddDate(0, 0, -7)
	}
	if endStr := ctx.Query("end_time"); endStr != "" {
		endTime, _ = time.Parse(time.RFC3339, endStr)
	} else {
		endTime = time.Now()
	}

	usage, err := c.irrigationService.GetZoneWaterUsage(startTime, endTime)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, usage)
}
