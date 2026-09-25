package services

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// DefaultWaterFlowRate 估算用水的默认流量（升/秒），与调度器完成时的估算保持一致
const DefaultWaterFlowRate = 0.1

// 冲突原因
const (
	// ConflictReasonZoneBusy 同区域已有进行中的灌溉任务
	ConflictReasonZoneBusy = "zone_busy"
	// ConflictReasonBudgetExceeded 当天累计用水 + 本次估算量超出每日限额
	ConflictReasonBudgetExceeded = "daily_budget_exceeded"
)

// ErrZoneNotFound 区域不存在
var ErrZoneNotFound = errors.New("zone not found")

// ConflictError 表示灌溉启动前的业务冲突（限额超限或区域忙碌）
type ConflictError struct {
	Reason    string
	Remaining *float64
}

func (e *ConflictError) Error() string {
	switch e.Reason {
	case ConflictReasonZoneBusy:
		return "zone already has an in-progress irrigation task"
	case ConflictReasonBudgetExceeded:
		return "estimated water usage exceeds the daily budget"
	default:
		return "irrigation conflict"
	}
}

// EstimateWaterUsage 根据时长估算用水量（升）
func EstimateWaterUsage(durationSeconds int) float64 {
	if durationSeconds <= 0 {
		return 0
	}
	return float64(durationSeconds) * DefaultWaterFlowRate
}

// CheckResult 灌溉前检查结果
type CheckResult struct {
	ZoneID      uint     `json:"zone_id"`
	UsedToday   float64  `json:"used_today"`
	Estimated   float64  `json:"estimated"`
	DailyBudget *float64 `json:"daily_budget"`
	Remaining   *float64 `json:"remaining"`
	InProgress  int64    `json:"in_progress"`
}

// ZoneBudgetStatus 区域每日水量限额及当日用量
type ZoneBudgetStatus struct {
	ZoneID      uint      `json:"zone_id"`
	ZoneName    string    `json:"zone_name"`
	DailyBudget *float64  `json:"daily_budget"`
	UsedToday   float64   `json:"used_today"`
	Remaining   *float64  `json:"remaining"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type BudgetService struct{}

func NewBudgetService() *BudgetService {
	return &BudgetService{}
}

// dayRange 返回本地时区下当天的起止时间
func dayRange(now time.Time) (time.Time, time.Time) {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return start, start.Add(24 * time.Hour)
}

// GetTodayWaterUsage 统计区域当天已完成灌溉的累计用水量（升）
func (s *BudgetService) GetTodayWaterUsage(db *gorm.DB, zoneID uint, now time.Time) (float64, error) {
	start, end := dayRange(now)
	var total float64
	err := db.Model(&models.IrrigationLog{}).
		Where("zone_id = ?", zoneID).
		Where("status = ?", models.ExecutionStatusSuccess).
		Where("start_time >= ? AND start_time < ?", start, end).
		Select("COALESCE(SUM(water_usage), 0)").
		Scan(&total).Error
	return total, err
}

// CountInProgress 统计区域当前进行中的灌溉任务数
func (s *BudgetService) CountInProgress(db *gorm.DB, zoneID uint) (int64, error) {
	var count int64
	err := db.Model(&models.IrrigationLog{}).
		Where("zone_id = ?", zoneID).
		Where("status = ?", models.ExecutionStatusInProgress).
		Count(&count).Error
	return count, err
}

// check 在给定事务内完成限额与进行中任务检查，返回组装好的检查结果
func (s *BudgetService) check(tx *gorm.DB, zoneID uint, estimated float64) (*CheckResult, error) {
	var zone models.IrrigationZone
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&zone, zoneID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrZoneNotFound
		}
		return nil, err
	}

	used, err := s.GetTodayWaterUsage(tx, zoneID, time.Now())
	if err != nil {
		return nil, err
	}

	inProgress, err := s.CountInProgress(tx, zoneID)
	if err != nil {
		return nil, err
	}

	result := &CheckResult{
		ZoneID:     zoneID,
		UsedToday:  used,
		Estimated:  estimated,
		InProgress: inProgress,
	}

	if zone.DailyWaterBudget != nil {
		remaining := *zone.DailyWaterBudget - used
		if remaining < 0 {
			remaining = 0
		}
		result.DailyBudget = zone.DailyWaterBudget
		result.Remaining = &remaining
	}

	if inProgress > 0 {
		return result, &ConflictError{Reason: ConflictReasonZoneBusy, Remaining: result.Remaining}
	}

	if zone.DailyWaterBudget != nil && used+estimated > *zone.DailyWaterBudget {
		return result, &ConflictError{Reason: ConflictReasonBudgetExceeded, Remaining: result.Remaining}
	}

	return result, nil
}

// CheckIrrigation 灌溉启动前的只读检查（供 /api/irrigation/check 调用）。
// 命中冲突时同时返回已组装的检查结果（含当日用量等）与 *ConflictError。
func (s *BudgetService) CheckIrrigation(zoneID uint, estimated float64) (*CheckResult, error) {
	var result *CheckResult
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		var checkErr error
		result, checkErr = s.check(tx, zoneID, estimated)
		return checkErr
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

// CheckAndStartIrrigation 在同一事务内完成检查并写入进行中的灌溉记录。
// 未设置每日限额的区域只受"同区域进行中任务"约束，限额检查自动跳过。
func (s *BudgetService) CheckAndStartIrrigation(logEntry *models.IrrigationLog, estimated float64) (*CheckResult, error) {
	var result *CheckResult
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		var checkErr error
		result, checkErr = s.check(tx, *logEntry.ZoneID, estimated)
		if checkErr != nil {
			return checkErr
		}

		if err := tx.Create(logEntry).Error; err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return &ConflictError{Reason: ConflictReasonZoneBusy, Remaining: result.Remaining}
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetBudget 查询区域限额配置及当天用量
func (s *BudgetService) GetBudget(zoneID uint) (*ZoneBudgetStatus, error) {
	var zone models.IrrigationZone
	if err := database.DB.First(&zone, zoneID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrZoneNotFound
		}
		return nil, err
	}

	used, err := s.GetTodayWaterUsage(database.DB, zoneID, time.Now())
	if err != nil {
		return nil, err
	}

	status := &ZoneBudgetStatus{
		ZoneID:      zone.ID,
		ZoneName:    zone.Name,
		DailyBudget: zone.DailyWaterBudget,
		UsedToday:   used,
		UpdatedAt:   zone.UpdatedAt,
	}
	if zone.DailyWaterBudget != nil {
		remaining := *zone.DailyWaterBudget - used
		if remaining < 0 {
			remaining = 0
		}
		status.Remaining = &remaining
	}
	return status, nil
}

// SetBudget 设置区域每日水量限额；传 nil 表示取消限额，恢复无限制运行
func (s *BudgetService) SetBudget(zoneID uint, budget *float64) (*ZoneBudgetStatus, error) {
	result := database.DB.Model(&models.IrrigationZone{}).
		Where("id = ?", zoneID).
		Update("daily_water_budget", budget)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrZoneNotFound
	}
	return s.GetBudget(zoneID)
}
