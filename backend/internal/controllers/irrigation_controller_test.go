package controllers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

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

func zoneRow(mock sqlmock.Sqlmock, id int64, budget interface{}) {
	cols := []string{"id", "name", "description", "daily_water_budget", "created_at", "updated_at", "deleted_at"}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM irrigation_zones`)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(id, "zone", "", budget, time.Now(), time.Now(), nil))
}

func usageSum(mock sqlmock.Sqlmock, used float64) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(SUM(water_usage), 0) FROM irrigation_logs`)).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(used))
}

func inProgressCount(mock sqlmock.Sqlmock, n int64) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM irrigation_logs`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
}

func performRequest(r http.Handler, method, path string, body string) *httptest.ResponseRecorder {
	var buf *bytes.Buffer
	if body != "" {
		buf = bytes.NewBufferString(body)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func parseBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json body %q: %v", w.Body.String(), err)
	}
	return body
}

// /api/irrigation/check 超限额 -> 409 + reason + remaining
func TestCheckEndpoint_BudgetExceeded(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	zoneRow(mock, 1, 100.0)
	usageSum(mock, 95)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	ctrl := NewIrrigationController()
	r.POST("/check", ctrl.Check)

	w := performRequest(r, http.MethodPost, "/check", `{"zone_id":1,"estimated_usage":10}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", w.Code, w.Body.String())
	}

	body := parseBody(t, w)
	if code, _ := body["code"].(float64); int(code) != 409 {
		t.Fatalf("expected body code 409, got %v", body["code"])
	}
	data, ok := body["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected data object, got %v", body["data"])
	}
	if data["reason"] != "budget_exceeded" {
		t.Fatalf("expected reason budget_exceeded, got %v", data["reason"])
	}
	if remaining, _ := data["remaining"].(float64); remaining != 5 {
		t.Fatalf("expected remaining 5, got %v", data["remaining"])
	}
	if used, _ := data["used_today"].(float64); used != 95 {
		t.Fatalf("expected used_today 95, got %v", data["used_today"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// /api/irrigation/check 在限额内且无冲突 -> 200
func TestCheckEndpoint_Allowed(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	zoneRow(mock, 1, 100.0)
	usageSum(mock, 10)
	inProgressCount(mock, 0)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	ctrl := NewIrrigationController()
	r.POST("/check", ctrl.Check)

	w := performRequest(r, http.MethodPost, "/check", `{"zone_id":1,"estimated_usage":5}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	body := parseBody(t, w)
	data := body["data"].(map[string]interface{})
	if allowed, _ := data["allowed"].(bool); !allowed {
		t.Fatalf("expected allowed=true, got %v", data)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// /api/irrigation/check 同区域进行中 -> 409 zone_busy
func TestCheckEndpoint_ZoneBusy(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	zoneRow(mock, 1, nil) // 未设限额：照旧运行但仍拦截任务冲突
	usageSum(mock, 0)
	inProgressCount(mock, 1)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	ctrl := NewIrrigationController()
	r.POST("/check", ctrl.Check)

	w := performRequest(r, http.MethodPost, "/check", `{"zone_id":1}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", w.Code, w.Body.String())
	}
	data := parseBody(t, w)["data"].(map[string]interface{})
	if data["reason"] != "zone_busy" {
		t.Fatalf("expected zone_busy, got %v", data["reason"])
	}
	if data["remaining"] != nil {
		t.Fatalf("expected nil remaining, got %v", data["remaining"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// /api/irrigation/manual 超限额 -> 409，且事务回滚、不创建任务
func TestManualEndpoint_BudgetExceeded(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	zoneRow(mock, 1, 100.0)
	usageSum(mock, 99)
	mock.ExpectRollback()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	ctrl := NewIrrigationController()
	r.POST("/manual", ctrl.ManualIrrigate)

	w := performRequest(r, http.MethodPost, "/manual", `{"zone_id":1,"estimated_usage":2}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", w.Code, w.Body.String())
	}
	data := parseBody(t, w)["data"].(map[string]interface{})
	if data["reason"] != "budget_exceeded" {
		t.Fatalf("expected budget_exceeded, got %v", data["reason"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// GET /api/zones/:id/budget 返回限额、当天用量和剩余
func TestGetBudgetEndpoint(t *testing.T) {
	mock, cleanup := newMockDB(t)
	defer cleanup()

	// GetZoneByID 使用 Preload(Devices)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM irrigation_zones`)).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "name", "description", "daily_water_budget", "created_at", "updated_at", "deleted_at"}).
			AddRow(1, "zone", "", 200.0, time.Now(), time.Now(), nil))
	mock.ExpectQuery(`SELECT \* FROM devices`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	usageSum(mock, 150)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	zoneCtrl := NewZoneController()
	r.GET("/zones/:id/budget", zoneCtrl.GetBudget)

	w := performRequest(r, http.MethodGet, "/zones/1/budget", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := parseBody(t, w)["data"].(map[string]interface{})
	if budget, _ := data["daily_water_budget"].(float64); budget != 200 {
		t.Fatalf("expected budget 200, got %v", data["daily_water_budget"])
	}
	if used, _ := data["used_today"].(float64); used != 150 {
		t.Fatalf("expected used_today 150, got %v", data["used_today"])
	}
	if remaining, _ := data["remaining"].(float64); remaining != 50 {
		t.Fatalf("expected remaining 50, got %v", data["remaining"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
