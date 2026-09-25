package controllers

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"

	"irrigation/internal/models"
	"irrigation/internal/services"
	"irrigation/pkg/response"
)

type ZoneController struct {
	zoneService   *services.ZoneService
	budgetService *services.BudgetService
}

func NewZoneController() *ZoneController {
	return &ZoneController{
		zoneService:   services.NewZoneService(),
		budgetService: services.NewBudgetService(),
	}
}

// ListZones godoc
// @Summary 获取灌溉区域列表
// @Description 获取所有灌溉区域
// @Tags 灌溉区域
// @Security ApiKeyAuth
// @Produce json
// @Success 200 {array} models.IrrigationZone
// @Router /api/zones [get]
func (c *ZoneController) List(ctx *gin.Context) {
	zones, err := c.zoneService.ListZones()
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}
	response.Success(ctx, zones)
}

// GetZone godoc
// @Summary 获取灌溉区域详情
// @Description 根据ID获取灌溉区域详情
// @Tags 灌溉区域
// @Security ApiKeyAuth
// @Produce json
// @Param id path int true "区域ID"
// @Success 200 {object} models.IrrigationZone
// @Router /api/zones/{id} [get]
func (c *ZoneController) Get(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)
	zone, err := c.zoneService.GetZoneByID(uint(id))
	if err != nil {
		response.NotFound(ctx, "Zone not found")
		return
	}
	response.Success(ctx, zone)
}

// CreateZone godoc
// @Summary 创建灌溉区域
// @Description 创建新的灌溉区域
// @Tags 灌溉区域
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param request body models.IrrigationZone true "区域信息"
// @Success 201 {object} models.IrrigationZone
// @Router /api/zones [post]
func (c *ZoneController) Create(ctx *gin.Context) {
	var zone models.IrrigationZone
	if err := ctx.ShouldBindJSON(&zone); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	if err := c.zoneService.CreateZone(&zone); err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Created(ctx, zone)
}

// UpdateZone godoc
// @Summary 更新灌溉区域
// @Description 更新灌溉区域信息
// @Tags 灌溉区域
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path int true "区域ID"
// @Param request body map[string]interface{} true "更新信息"
// @Success 200 {object} response.Response
// @Router /api/zones/{id} [put]
func (c *ZoneController) Update(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	var updates map[string]interface{}
	if err := ctx.ShouldBindJSON(&updates); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	if err := c.zoneService.UpdateZone(uint(id), updates); err != nil {
		response.NotFound(ctx, err.Error())
		return
	}

	response.Success(ctx, nil)
}

// DeleteZone godoc
// @Summary 删除灌溉区域
// @Description 删除灌溉区域
// @Tags 灌溉区域
// @Security ApiKeyAuth
// @Produce json
// @Param id path int true "区域ID"
// @Success 200 {object} response.Response
// @Router /api/zones/{id} [delete]
func (c *ZoneController) Delete(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	if err := c.zoneService.DeleteZone(uint(id)); err != nil {
		response.NotFound(ctx, err.Error())
		return
	}

	response.Success(ctx, nil)
}

// GetBudget godoc
// @Summary 获取区域每日水量限额
// @Description 获取指定区域的每日水量限额、当天累计用水量与剩余额度；未设置限额时 daily_budget/remaining 为 null
// @Tags 灌溉区域
// @Security ApiKeyAuth
// @Produce json
// @Param id path int true "区域ID"
// @Success 200 {object} services.ZoneBudgetStatus
// @Router /api/zones/{id}/budget [get]
func (c *ZoneController) GetBudget(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	status, err := c.budgetService.GetBudget(uint(id))
	if err != nil {
		if errors.Is(err, services.ErrZoneNotFound) {
			response.NotFound(ctx, "Zone not found")
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, status)
}

// UpdateBudget godoc
// @Summary 设置区域每日水量限额
// @Description 设置指定区域的每日水量限额（升）；传 null 取消限额，区域恢复无限制运行
// @Tags 灌溉区域
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path int true "区域ID"
// @Param request body object true "限额设置，如 {\"daily_water_budget\": 500}"
// @Success 200 {object} services.ZoneBudgetStatus
// @Failure 400 {object} response.Response
// @Router /api/zones/{id}/budget [put]
func (c *ZoneController) UpdateBudget(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	var body map[string]interface{}
	if err := ctx.ShouldBindJSON(&body); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	value, exists := body["daily_water_budget"]
	if !exists {
		response.BadRequest(ctx, "daily_water_budget is required")
		return
	}

	// 显式传 null 表示取消限额
	if value == nil {
		status, err := c.budgetService.SetBudget(uint(id), nil)
		if err != nil {
			if errors.Is(err, services.ErrZoneNotFound) {
				response.NotFound(ctx, "Zone not found")
				return
			}
			response.InternalServerError(ctx, err.Error())
			return
		}
		response.Success(ctx, status)
		return
	}

	budget, ok := value.(float64)
	if !ok || budget < 0 {
		response.BadRequest(ctx, "daily_water_budget must be a non-negative number")
		return
	}

	status, err := c.budgetService.SetBudget(uint(id), &budget)
	if err != nil {
		if errors.Is(err, services.ErrZoneNotFound) {
			response.NotFound(ctx, "Zone not found")
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, status)
}
