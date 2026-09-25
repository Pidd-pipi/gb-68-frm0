package services

import (
	"time"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

type IrrigationService struct {
	budgetService *BudgetService
}

func NewIrrigationService() *IrrigationService {
	return &IrrigationService{
		budgetService: NewBudgetService(),
	}
}

// StartIrrigationOptions 启动灌溉时的可选参数
type StartIrrigationOptions struct {
	// EstimatedUsage 本次灌溉的预估用水量（升），> 0 时优先使用
	EstimatedUsage float64
	// DurationSeconds 本次灌溉预计时长（秒），未显式给估算量时按时长估算
	DurationSeconds int
}

// StartIrrigation 启动灌溉。启动前统一走每日限额与同区域进行中任务检查：
// 超出每日限额或同区域已有进行中任务时返回 *ConflictError；
// 没有区域（zoneID 为 nil）或区域未设置限额时照旧运行。
func (s *IrrigationService) StartIrrigation(scheduleID *uint, zoneID *uint, triggerType models.TriggerType, opts *StartIrrigationOptions) (*models.IrrigationLog, error) {
	logEntry := &models.IrrigationLog{
		ScheduleID:  scheduleID,
		ZoneID:      zoneID,
		TriggerType: triggerType,
		StartTime:   time.Now(),
		Status:      models.ExecutionStatusInProgress,
	}

	// 未绑定区域的灌溉任务不做区域级限额/冲突检查，保持原有行为
	if zoneID == nil {
		if err := database.DB.Create(logEntry).Error; err != nil {
			return nil, err
		}
		return logEntry, nil
	}

	estimated := 0.0
	if opts != nil {
		estimated = opts.EstimatedUsage
		if estimated <= 0 {
			estimated = EstimateWaterUsage(opts.DurationSeconds)
		}
	}

	if _, err := s.budgetService.CheckAndStartIrrigation(logEntry, estimated); err != nil {
		return nil, err
	}
	return logEntry, nil
}

func (s *IrrigationService) CompleteIrrigation(logID uint, success bool, waterUsage *float64, errorMsg *string) error {
	now := time.Now()
	updates := map[string]interface{}{
		"end_time": now,
	}

	if success {
		updates["status"] = models.ExecutionStatusSuccess
	} else {
		updates["status"] = models.ExecutionStatusFailed
		if errorMsg != nil {
			updates["error_message"] = *errorMsg
		}
	}

	if waterUsage != nil {
		updates["water_usage"] = *waterUsage
	}

	return database.DB.Model(&models.IrrigationLog{}).
		Where("id = ?", logID).
		Updates(updates).Error
}

func (s *IrrigationService) GetIrrigationHistory(zoneID *uint, startTime, endTime time.Time, limit int) ([]models.IrrigationLog, error) {
	var logs []models.IrrigationLog
	query := database.DB

	if zoneID != nil {
		query = query.Where("zone_id = ?", *zoneID)
	}
	if !startTime.IsZero() {
		query = query.Where("start_time >= ?", startTime)
	}
	if !endTime.IsZero() {
		query = query.Where("start_time <= ?", endTime)
	}

	if limit > 0 {
		query = query.Limit(limit)
	}

	if err := query.Order("start_time DESC").Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

type WaterUsageStats struct {
	TotalUsage      float64 `json:"total_usage"`
	Duration        int64   `json:"duration"`
	IrrigationCount int64   `json:"irrigation_count"`
}

func (s *IrrigationService) GetWaterUsageStats(zoneID *uint, startTime, endTime time.Time) (*WaterUsageStats, error) {
	var stats WaterUsageStats
	query := database.DB.Model(&models.IrrigationLog{}).
		Select("COALESCE(SUM(water_usage), 0) as total_usage, COALESCE(COUNT(*), 0) as irrigation_count").
		Where("status = ?", models.ExecutionStatusSuccess)

	if zoneID != nil {
		query = query.Where("zone_id = ?", *zoneID)
	}
	if !startTime.IsZero() {
		query = query.Where("start_time >= ?", startTime)
	}
	if !endTime.IsZero() {
		query = query.Where("start_time <= ?", endTime)
	}

	err := query.Scan(&stats).Error
	return &stats, err
}

type ZoneWaterUsage struct {
	ZoneID     uint    `json:"zone_id"`
	ZoneName   string  `json:"zone_name"`
	WaterUsage float64 `json:"water_usage"`
	Percentage float64 `json:"percentage"`
}

func (s *IrrigationService) GetZoneWaterUsage(startTime, endTime time.Time) ([]ZoneWaterUsage, error) {
	var zoneUsages []ZoneWaterUsage

	query := `
		SELECT
			z.id as zone_id,
			z.name as zone_name,
			COALESCE(SUM(il.water_usage), 0) as water_usage
		FROM irrigation_zones z
		LEFT JOIN irrigation_logs il ON z.id = il.zone_id
			AND il.status = 'success'
			AND il.start_time >= ?
			AND il.start_time <= ?
		GROUP BY z.id, z.name
		ORDER BY water_usage DESC
	`

	err := database.DB.Raw(query, startTime, endTime).Scan(&zoneUsages).Error
	if err != nil {
		return nil, err
	}

	var total float64
	for _, zu := range zoneUsages {
		total += zu.WaterUsage
	}

	if total > 0 {
		for i := range zoneUsages {
			zoneUsages[i].Percentage = (zoneUsages[i].WaterUsage / total) * 100
		}
	}

	return zoneUsages, nil
}
