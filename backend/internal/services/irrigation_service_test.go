package services

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

func newMockDB(t *testing.T) (sqlmock.Sqlmock, func()) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB, WithoutQuotingCheck: true}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	old := database.DB
	database.DB = gdb
	return mock, func() {
		database.DB = old
		_ = sqlDB.Close()
	}
}

func zoneColumns() []string {
	return []string{"id", "name", "description", "daily_water_budget", "created_at", "updated_at", "deleted_at"}
}

func expectZoneRowFor(mock sqlmock.Sqlmock, id int64, budget interface{}) {
	rows := sqlmock.NewRows(zoneColumns()).
		AddRow(id, "zone", "", budget, time.Now(), time.Now(), nil)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM irrigation_zones`)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(rows)
}

func expectZoneNotFound(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM irrigation_zones`)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(gorm.ErrRecordNotFound)
}

func expectUsageSum(mock sqlmock.Sqlmock, used float64) {
	rows := sqlmock.NewRows([]string{"coalesce"}).AddRow(used)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(SUM(water_usage), 0) FROM irrigation_logs`)).
		WillReturnRows(rows)
}

func expectInProgressCount(mock sqlmock.Sqlmock, count int64) {
	rows := sqlmock.NewRows([]string{"count"}).AddRow(count)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM irrigation_logs`)).
		WillReturnRows(rows)
}

func expectAdvisoryLock(mock sqlmock.Sqlmock, zoneID int64) {
	mock.ExpectExec(`pg_advisory_xact_lock`).
		WithArgs(advisoryLockNamespace, zoneID).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

func expectInsertLog(mock sqlmock.Sqlmock, id int64) {
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO irrigation_logs`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(id))
}

// 没设限额、没有进行中任务 -> 放行
func TestCheckIrrigation_NoBudget_Allowed(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	expectZoneRowFor(mock, 1, nil)
	expectUsageSum(mock, 30)
	expectInProgressCount(mock, 0)

	res, err := NewIrrigationService().CheckIrrigation(1, 100)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.Allowed {
		t.Fatalf("expected allowed, reason=%s", res.Reason)
	}
	if res.Remaining != nil {
		t.Fatalf("expected nil remaining, got %v", *res.Remaining)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 没设限额但同区域有进行中任务 -> zone_busy
func TestCheckIrrigation_NoBudget_ZoneBusy(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	expectZoneRowFor(mock, 1, nil)
	expectUsageSum(mock, 30)
	expectInProgressCount(mock, 1)

	res, err := NewIrrigationService().CheckIrrigation(1, 0)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Allowed || res.Reason != CheckReasonZoneBusy {
		t.Fatalf("expected zone_busy, got allowed=%v reason=%s", res.Allowed, res.Reason)
	}
	if res.Remaining != nil {
		t.Fatalf("expected nil remaining for no-budget zone")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 限额内 -> 放行并给出 remaining
func TestCheckIrrigation_WithinBudget(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	expectZoneRowFor(mock, 1, 100.0)
	expectUsageSum(mock, 30.0)
	expectInProgressCount(mock, 0)

	res, err := NewIrrigationService().CheckIrrigation(1, 20.5)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.Allowed {
		t.Fatalf("expected allowed, reason=%s", res.Reason)
	}
	if res.Remaining == nil || *res.Remaining != 70.0 {
		t.Fatalf("expected remaining 70, got %v", res.Remaining)
	}
	if res.UsedToday != 30 {
		t.Fatalf("expected used_today 30, got %v", res.UsedToday)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 当天累计 + 估算超出限额 -> budget_exceeded，优先于任务冲突判断
func TestCheckIrrigation_BudgetExceeded(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	expectZoneRowFor(mock, 1, 100.0)
	expectUsageSum(mock, 95.0)

	res, err := NewIrrigationService().CheckIrrigation(1, 10)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Allowed || res.Reason != CheckReasonBudgetExceeded {
		t.Fatalf("expected budget_exceeded, got allowed=%v reason=%s", res.Allowed, res.Reason)
	}
	if res.Remaining == nil || *res.Remaining != 5.0 {
		t.Fatalf("expected remaining 5, got %v", res.Remaining)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 区域不存在 -> ErrZoneNotFound
func TestCheckIrrigation_ZoneNotFound(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	expectZoneNotFound(mock)

	_, err := NewIrrigationService().CheckIrrigation(1, 10)
	if !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("expected ErrZoneNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// StartIrrigationChecked: 先拿区域 advisory lock，检查通过后在同事务内插入任务
func TestStartIrrigationChecked_CommitsAndLocks(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	expectAdvisoryLock(mock, 1)
	expectZoneRowFor(mock, 1, 100.0)
	expectUsageSum(mock, 10.0)
	expectInProgressCount(mock, 0)
	expectInsertLog(mock, 7)
	mock.ExpectCommit()

	zoneID := uint(1)
	log, err := NewIrrigationService().StartIrrigationChecked(nil, &zoneID, models.TriggerTypeManual, 5)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if log.ID != 7 || log.Status != models.ExecutionStatusInProgress {
		t.Fatalf("unexpected log: %+v", log)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// StartIrrigationChecked: 超出限额时返回 CheckConflictError 且事务回滚、不插入任务
func TestStartIrrigationChecked_BudgetConflictRollsBack(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	expectAdvisoryLock(mock, 1)
	expectZoneRowFor(mock, 1, 100.0)
	expectUsageSum(mock, 99.0)
	mock.ExpectRollback()

	zoneID := uint(1)
	_, err := NewIrrigationService().StartIrrigationChecked(nil, &zoneID, models.TriggerTypeTimed, 2)
	var conflictErr *CheckConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected CheckConflictError, got %v", err)
	}
	if conflictErr.Result.Reason != CheckReasonBudgetExceeded {
		t.Fatalf("expected budget_exceeded, got %s", conflictErr.Result.Reason)
	}
	if conflictErr.Result.Remaining == nil || *conflictErr.Result.Remaining != 1 {
		t.Fatalf("expected remaining 1, got %v", conflictErr.Result.Remaining)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// StartIrrigationChecked: 同区域进行中任务 -> zone_busy 并回滚
func TestStartIrrigationChecked_ZoneBusyRollsBack(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	expectAdvisoryLock(mock, 2)
	expectZoneRowFor(mock, 2, nil)
	expectUsageSum(mock, 0)
	expectInProgressCount(mock, 1)
	mock.ExpectRollback()

	zoneID := uint(2)
	_, err := NewIrrigationService().StartIrrigationChecked(nil, &zoneID, models.TriggerTypeConditional, 0)
	var conflictErr *CheckConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected CheckConflictError, got %v", err)
	}
	if conflictErr.Result.Reason != CheckReasonZoneBusy {
		t.Fatalf("expected zone_busy, got %s", conflictErr.Result.Reason)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// zoneID 为空（无区域的计划）时走普通启动，不加锁、不检查
func TestStartIrrigationChecked_NoZoneSkipsLock(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	expectInsertLog(mock, 3)
	mock.ExpectCommit()

	log, err := NewIrrigationService().StartIrrigationChecked(nil, nil, models.TriggerTypeTimed, 0)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if log.ID != 3 {
		t.Fatalf("unexpected log id: %d", log.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
