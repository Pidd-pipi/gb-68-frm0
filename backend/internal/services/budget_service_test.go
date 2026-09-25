package services

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

func newMockDB(t *testing.T) (sqlmock.Sqlmock, func()) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	gdb, err := gorm.Open(postgres.New(postgres.Config{
		Conn:                 sqlDB,
		PreferSimpleProtocol: true,
	}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	old := database.DB
	database.DB = gdb
	return mock, func() {
		database.DB = old
		_ = sqlDB.Close()
	}
}

var (
	zoneSelectRe  = regexp.MustCompile(`(?is)SELECT .*"irrigation_zones".*FOR UPDATE`)
	sumUsageRe    = regexp.MustCompile(`(?is)SELECT COALESCE\(SUM\(water_usage\).*"irrigation_logs"`)
	countActiveRe = regexp.MustCompile(`(?is)SELECT count\(\*\).*"irrigation_logs"`)
	insertLogRe   = regexp.MustCompile(`(?is)INSERT INTO "irrigation_logs"`)
)

func zoneColumns() []string {
	return []string{"id", "name", "description", "daily_water_budget", "created_at", "updated_at", "deleted_at"}
}

func expectZoneAndAggs(mock sqlmock.Sqlmock, budget interface{}, used float64, inProgress int64) {
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(zoneSelectRe.String()).
		WithArgs(uint(1), 1).
		WillReturnRows(sqlmock.NewRows(zoneColumns()).
			AddRow(1, "A区", "desc", budget, now, now, nil))
	mock.ExpectQuery(sumUsageRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(used))
	mock.ExpectQuery(countActiveRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(inProgress))
	mock.ExpectCommit()
}

// 未设限额且没有进行中任务：放行，remaining 为 nil
func TestCheck_NoBudget_Allowed(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()
	expectZoneAndAggs(mock, nil, 80, 0)

	svc := NewBudgetService()
	res, err := svc.CheckIrrigation(1, 100)
	if err != nil {
		t.Fatalf("expected allowed, got %v", err)
	}
	if res.DailyBudget != nil || res.Remaining != nil {
		t.Fatalf("unlimited zone should have nil budget/remaining, got %+v", res)
	}
	if res.UsedToday != 80 || res.Estimated != 100 {
		t.Fatalf("unexpected result %+v", res)
	}
}

// 设置限额且用量未超：放行，remaining = 预算 - 已用
func TestCheck_WithinBudget_Allowed(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()
	expectZoneAndAggs(mock, 100.0, 60, 0)

	svc := NewBudgetService()
	res, err := svc.CheckIrrigation(1, 30)
	if err != nil {
		t.Fatalf("expected allowed, got %v", err)
	}
	if res.Remaining == nil || *res.Remaining != 40 {
		t.Fatalf("expected remaining 40, got %+v", res.Remaining)
	}
}

// 超出限额：409 类错误 daily_budget_exceeded + remaining
func TestCheck_BudgetExceeded_Conflict(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()
	expectZoneAndAggs(mock, 100.0, 80, 0)

	svc := NewBudgetService()
	_, err := svc.CheckIrrigation(1, 30)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError, got %v", err)
	}
	if conflict.Reason != ConflictReasonBudgetExceeded {
		t.Fatalf("expected %s, got %s", ConflictReasonBudgetExceeded, conflict.Reason)
	}
	if conflict.Remaining == nil || *conflict.Remaining != 20 {
		t.Fatalf("expected remaining 20, got %v", conflict.Remaining)
	}
}

// 进行中任务优先判定为 zone_busy（即使同时超额）
func TestCheck_ZoneBusy_Conflict(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()
	expectZoneAndAggs(mock, 100.0, 200, 1)

	svc := NewBudgetService()
	_, err := svc.CheckIrrigation(1, 30)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError, got %v", err)
	}
	if conflict.Reason != ConflictReasonZoneBusy {
		t.Fatalf("expected %s, got %s", ConflictReasonZoneBusy, conflict.Reason)
	}
}

// 区域不存在
func TestCheck_ZoneNotFound(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectQuery(zoneSelectRe.String()).
		WithArgs(uint(99), 1).
		WillReturnError(gorm.ErrRecordNotFound)
	mock.ExpectRollback()

	svc := NewBudgetService()
	_, err := svc.CheckIrrigation(99, 1)
	if !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("expected ErrZoneNotFound, got %v", err)
	}
}

// 写入进行中记录时命中部分唯一索引（23505）→ zone_busy，且事务回滚
func TestCheckAndStart_UniqueViolation_ZoneBusy(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(zoneSelectRe.String()).
		WithArgs(uint(1), 1).
		WillReturnRows(sqlmock.NewRows(zoneColumns()).
			AddRow(1, "A区", "desc", nil, now, now, nil))
	mock.ExpectQuery(sumUsageRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))
	mock.ExpectQuery(countActiveRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(insertLogRe.String()).
		WillReturnError(&pgconn.PgError{Code: "23505"})
	mock.ExpectRollback()

	zoneID := uint(1)
	logEntry := &models.IrrigationLog{ZoneID: &zoneID, TriggerType: models.TriggerTypeManual, StartTime: now, Status: models.ExecutionStatusInProgress}
	_, err := NewBudgetService().CheckAndStartIrrigation(logEntry, 10)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError, got %v", err)
	}
	if conflict.Reason != ConflictReasonZoneBusy {
		t.Fatalf("expected %s, got %s", ConflictReasonZoneBusy, conflict.Reason)
	}
}

// 检查通过且写入成功
func TestCheckAndStart_Success(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(zoneSelectRe.String()).
		WithArgs(uint(1), 1).
		WillReturnRows(sqlmock.NewRows(zoneColumns()).
			AddRow(1, "A区", "desc", 100.0, now, now, nil))
	mock.ExpectQuery(sumUsageRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(10))
	mock.ExpectQuery(countActiveRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(insertLogRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectCommit()

	zoneID := uint(1)
	logEntry := &models.IrrigationLog{ZoneID: &zoneID, TriggerType: models.TriggerTypeManual, StartTime: now, Status: models.ExecutionStatusInProgress}
	res, err := NewBudgetService().CheckAndStartIrrigation(logEntry, 10)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if logEntry.ID != 1 {
		t.Fatalf("expected log id 1, got %d", logEntry.ID)
	}
	if res.Remaining == nil || *res.Remaining != 90 {
		t.Fatalf("expected remaining 90, got %v", res.Remaining)
	}
}

func TestEstimateWaterUsage(t *testing.T) {
	if got := EstimateWaterUsage(600); got != 60 {
		t.Fatalf("expected 60, got %v", got)
	}
	if got := EstimateWaterUsage(0); got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
}
