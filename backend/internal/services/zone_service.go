package services

import (
	"errors"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

type ZoneService struct{}

// ZoneBudgetStatus 区域每日水量限额状态
type ZoneBudgetStatus struct {
	ZoneID           uint     `json:"zone_id"`
	DailyWaterBudget *float64 `json:"daily_water_budget"`
	UsedToday        float64  `json:"used_today"`
	Remaining        *float64 `json:"remaining"`
}

// SetBudgetRequest 设置区域每日水量限额的请求体，daily_water_budget 为 null 表示清除限额
type SetBudgetRequest struct {
	DailyWaterBudget *float64 `json:"daily_water_budget"`
}

func NewZoneService() *ZoneService {
	return &ZoneService{}
}

func (s *ZoneService) CreateZone(zone *models.IrrigationZone) error {
	return database.DB.Create(zone).Error
}

func (s *ZoneService) GetZoneByID(id uint) (*models.IrrigationZone, error) {
	var zone models.IrrigationZone
	if err := database.DB.Preload("Devices").First(&zone, id).Error; err != nil {
		return nil, err
	}
	return &zone, nil
}

func (s *ZoneService) ListZones() ([]models.IrrigationZone, error) {
	var zones []models.IrrigationZone
	if err := database.DB.Preload("Devices").Find(&zones).Error; err != nil {
		return nil, err
	}
	return zones, nil
}

func (s *ZoneService) UpdateZone(id uint, updates map[string]interface{}) error {
	result := database.DB.Model(&models.IrrigationZone{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("zone not found")
	}
	return nil
}

func (s *ZoneService) DeleteZone(id uint) error {
	result := database.DB.Delete(&models.IrrigationZone{}, id)
	if result.RowsAffected == 0 {
		return errors.New("zone not found")
	}
	return result.Error
}

// SetZoneBudget 设置区域每日水量限额，传 nil 表示清除限额（不限制）
func (s *ZoneService) SetZoneBudget(id uint, budget *float64) error {
	result := database.DB.Model(&models.IrrigationZone{}).
		Where("id = ?", id).
		Update("daily_water_budget", budget)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("zone not found")
	}
	return nil
}

// GetZoneBudgetStatus 返回区域限额及当天累计用水、剩余额度
func (s *ZoneService) GetZoneBudgetStatus(id uint) (*ZoneBudgetStatus, error) {
	zone, err := s.GetZoneByID(id)
	if err != nil {
		return nil, err
	}

	usedToday, err := NewIrrigationService().GetZoneUsageToday(id)
	if err != nil {
		return nil, err
	}

	status := &ZoneBudgetStatus{
		ZoneID:           id,
		DailyWaterBudget: zone.DailyWaterBudget,
		UsedToday:        usedToday,
	}
	if zone.DailyWaterBudget != nil {
		remaining := round2(*zone.DailyWaterBudget - usedToday)
		status.Remaining = &remaining
	}

	return status, nil
}
