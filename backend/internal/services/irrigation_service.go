package services

import (
	"errors"
	"math"
	"time"

	"gorm.io/gorm"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

var ErrZoneNotFound = errors.New("zone not found")

// 灌溉前检查不通过的原因
const (
	CheckReasonBudgetExceeded = "budget_exceeded" // 超出区域每日水量限额
	CheckReasonZoneBusy       = "zone_busy"       // 同区域已有进行中的灌溉任务
)

type IrrigationService struct{}

func NewIrrigationService() *IrrigationService {
	return &IrrigationService{}
}

// IrrigationCheckResult 灌溉前检查结果
type IrrigationCheckResult struct {
	Allowed        bool     `json:"allowed"`
	Reason         string   `json:"reason,omitempty"`
	ZoneID         uint     `json:"zone_id"`
	DailyBudget    *float64 `json:"daily_water_budget"`
	UsedToday      float64  `json:"used_today"`
	EstimatedUsage float64  `json:"estimated_usage"`
	Remaining      *float64 `json:"remaining"`
}

// CheckIrrigationRequest 灌溉前检查请求
type CheckIrrigationRequest struct {
	ZoneID         uint    `json:"zone_id" binding:"required"`
	EstimatedUsage float64 `json:"estimated_usage"`
}

// ManualIrrigationRequest 手动灌溉请求，EstimatedUsage 为本次估算用水量（可选，用于限额检查）
type ManualIrrigationRequest struct {
	ZoneID         uint     `json:"zone_id" binding:"required"`
	EstimatedUsage *float64 `json:"estimated_usage"`
}

// CheckConflictError 灌溉前检查未通过，携带完整检查结果（reason、remaining 等）
type CheckConflictError struct {
	Result *IrrigationCheckResult
}

func (e *CheckConflictError) Error() string {
	return "irrigation not allowed: " + e.Result.Reason
}

// CheckIrrigation 在计划或手动灌溉启动前检查：
// 同区域已有进行中任务、或当天累计用水 + 本次估算量超出每日限额时不允许启动。
// 未设置限额的区域只检查任务冲突，其余照旧运行。
func (s *IrrigationService) CheckIrrigation(zoneID uint, estimatedUsage float64) (*IrrigationCheckResult, error) {
	return s.checkIrrigation(database.DB, zoneID, estimatedUsage)
}

func (s *IrrigationService) checkIrrigation(db *gorm.DB, zoneID uint, estimatedUsage float64) (*IrrigationCheckResult, error) {
	var zone models.IrrigationZone
	if err := db.First(&zone, zoneID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrZoneNotFound
		}
		return nil, err
	}

	usedToday, err := s.getZoneUsageToday(db, zoneID)
	if err != nil {
		return nil, err
	}

	result := &IrrigationCheckResult{
		Allowed:        true,
		ZoneID:         zoneID,
		DailyBudget:    zone.DailyWaterBudget,
		UsedToday:      usedToday,
		EstimatedUsage: estimatedUsage,
	}

	if zone.DailyWaterBudget != nil {
		remaining := round2(*zone.DailyWaterBudget - usedToday)
		result.Remaining = &remaining
		if usedToday+estimatedUsage > *zone.DailyWaterBudget {
			result.Allowed = false
			result.Reason = CheckReasonBudgetExceeded
			return result, nil
		}
	}

	busy, err := s.hasInProgressIrrigation(db, zoneID)
	if err != nil {
		return nil, err
	}
	if busy {
		result.Allowed = false
		result.Reason = CheckReasonZoneBusy
	}

	return result, nil
}

// advisoryLockNamespace 区域级灌溉咨询锁的命名空间
const advisoryLockNamespace int64 = 23118

// StartIrrigationChecked 在一个事务内完成"限额/冲突检查 + 创建进行中任务"。
// 通过区域级 advisory lock 串行化同区域的并发启动（如同一分钟触发的多个计划），
// 消除检查与创建之间的竞态窗口。检查不通过返回 *CheckConflictError。
func (s *IrrigationService) StartIrrigationChecked(scheduleID *uint, zoneID *uint, triggerType models.TriggerType, estimatedUsage float64) (*models.IrrigationLog, error) {
	if zoneID == nil {
		return s.StartIrrigation(scheduleID, zoneID, triggerType)
	}

	var log *models.IrrigationLog
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?::int, ?::int)", advisoryLockNamespace, int64(*zoneID)).Error; err != nil {
			return err
		}

		result, err := s.checkIrrigation(tx, *zoneID, estimatedUsage)
		if err != nil {
			return err
		}
		if !result.Allowed {
			return &CheckConflictError{Result: result}
		}

		log = &models.IrrigationLog{
			ScheduleID:  scheduleID,
			ZoneID:      zoneID,
			TriggerType: triggerType,
			StartTime:   time.Now(),
			Status:      models.ExecutionStatusInProgress,
		}
		return tx.Create(log).Error
	})
	if err != nil {
		return nil, err
	}

	return log, nil
}

// GetZoneUsageToday 返回区域当天（按服务器本地时区）已成功灌溉的累计用水量
func (s *IrrigationService) GetZoneUsageToday(zoneID uint) (float64, error) {
	return s.getZoneUsageToday(database.DB, zoneID)
}

func (s *IrrigationService) getZoneUsageToday(db *gorm.DB, zoneID uint) (float64, error) {
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	var used float64
	err := db.Model(&models.IrrigationLog{}).
		Select("COALESCE(SUM(water_usage), 0)").
		Where("zone_id = ? AND status = ? AND start_time >= ?", zoneID, models.ExecutionStatusSuccess, startOfDay).
		Scan(&used).Error
	return round2(used), err
}

// HasInProgressIrrigation 判断区域当前是否有进行中的灌溉任务
func (s *IrrigationService) HasInProgressIrrigation(zoneID uint) (bool, error) {
	return s.hasInProgressIrrigation(database.DB, zoneID)
}

func (s *IrrigationService) hasInProgressIrrigation(db *gorm.DB, zoneID uint) (bool, error) {
	var count int64
	err := db.Model(&models.IrrigationLog{}).
		Where("zone_id = ? AND status = ?", zoneID, models.ExecutionStatusInProgress).
		Count(&count).Error
	return count > 0, err
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func (s *IrrigationService) StartIrrigation(scheduleID *uint, zoneID *uint, triggerType models.TriggerType) (*models.IrrigationLog, error) {
	log := &models.IrrigationLog{
		ScheduleID:  scheduleID,
		ZoneID:      zoneID,
		TriggerType: triggerType,
		StartTime:   time.Now(),
		Status:      models.ExecutionStatusInProgress,
	}

	if err := database.DB.Create(log).Error; err != nil {
		return nil, err
	}

	return log, nil
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
	TotalUsage   float64 `json:"total_usage"`
	Duration     int64   `json:"duration"`
	IrrigationCount int64 `json:"irrigation_count"`
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
