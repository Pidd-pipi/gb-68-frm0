package controllers

import (
	"bytes"
	"database/sql"
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

	"irrigation/pkg/database"
)

func gormOpenTestDB(t *testing.T, sqlDB *sql.DB) (*gorm.DB, error) {
	t.Helper()
	return gorm.Open(postgres.New(postgres.Config{
		Conn:                 sqlDB,
		PreferSimpleProtocol: true,
	}), &gorm.Config{})
}

func newTestRouter(t *testing.T) (sqlmock.Sqlmock, *gin.Engine, func()) {
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

	gin.SetMode(gin.TestMode)
	r := gin.New()
	ic := NewIrrigationController()
	r.POST("/api/irrigation/check", ic.CheckIrrigation)
	r.POST("/api/irrigation/manual", ic.ManualIrrigate)

	return mock, r, func() {
		database.DB = old
		_ = sqlDB.Close()
	}
}

var (
	httpZoneSelectRe  = regexp.MustCompile(`(?is)SELECT .*"irrigation_zones".*FOR UPDATE`)
	httpSumUsageRe    = regexp.MustCompile(`(?is)SELECT COALESCE\(SUM\(water_usage\).*"irrigation_logs"`)
	httpCountActiveRe = regexp.MustCompile(`(?is)SELECT count\(\*\).*"irrigation_logs"`)
	httpInsertLogRe   = regexp.MustCompile(`(?is)INSERT INTO "irrigation_logs"`)
)

func expectCheckAggs(mock sqlmock.Sqlmock, budget interface{}, used float64, inProgress int64) {
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(httpZoneSelectRe.String()).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "name", "description", "daily_water_budget", "created_at", "updated_at", "deleted_at"}).
			AddRow(1, "A区", "desc", budget, now, now, nil))
	mock.ExpectQuery(httpSumUsageRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(used))
	mock.ExpectQuery(httpCountActiveRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(inProgress))
	mock.ExpectCommit()
}

func doJSON(t *testing.T, r *gin.Engine, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// 超额 → 409，含 reason=daily_budget_exceeded 和 remaining
func TestCheckEndpoint_BudgetExceeded_409(t *testing.T) {
	mock, r, cleanup := newTestRouter(t)
	defer cleanup()
	expectCheckAggs(mock, 100.0, 80, 0)

	w := doJSON(t, r, "/api/irrigation/check", `{"zone_id":1,"duration":300}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var data map[string]interface{}
	json.Unmarshal(resp.Data, &data)
	if data["reason"] != "daily_budget_exceeded" {
		t.Fatalf("expected reason daily_budget_exceeded, got %v", data["reason"])
	}
	if remaining, ok := data["remaining"].(float64); !ok || remaining != 20 {
		t.Fatalf("expected remaining 20, got %#v", data["remaining"])
	}
	if estimated, ok := data["estimated"].(float64); !ok || estimated != 30 {
		t.Fatalf("expected estimated 30 (300s*0.1), got %#v", data["estimated"])
	}
}

// 同区域进行中 → 409 zone_busy，未设限额时 remaining 为 null
func TestCheckEndpoint_ZoneBusy_409_NullRemaining(t *testing.T) {
	mock, r, cleanup := newTestRouter(t)
	defer cleanup()
	expectCheckAggs(mock, nil, 0, 1)

	w := doJSON(t, r, "/api/irrigation/check", `{"zone_id":1,"duration":300}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Data["reason"] != "zone_busy" {
		t.Fatalf("expected zone_busy, got %v", resp.Data["reason"])
	}
	if _, present := resp.Data["remaining"]; !present {
		t.Fatal("remaining field missing")
	}
	if resp.Data["remaining"] != nil {
		t.Fatalf("expected null remaining for unlimited zone, got %v", resp.Data["remaining"])
	}
}

// 未设限额、无进行中 → 200 放行
func TestCheckEndpoint_NoBudget_200(t *testing.T) {
	mock, r, cleanup := newTestRouter(t)
	defer cleanup()
	expectCheckAggs(mock, nil, 80, 0)

	w := doJSON(t, r, "/api/irrigation/check", `{"zone_id":1,"estimated_usage":500}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Data["remaining"] != nil {
		t.Fatalf("expected null remaining, got %v", resp.Data["remaining"])
	}
	if resp.Data["used_today"].(float64) != 80 || resp.Data["estimated"].(float64) != 500 {
		t.Fatalf("unexpected data: %+v", resp.Data)
	}
}

// 手动灌溉超额同样 409（调度器与手动入口共用同一判断）
func TestManualEndpoint_BudgetExceeded_409(t *testing.T) {
	mock, r, cleanup := newTestRouter(t)
	defer cleanup()
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(httpZoneSelectRe.String()).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "name", "description", "daily_water_budget", "created_at", "updated_at", "deleted_at"}).
			AddRow(1, "A区", "desc", 100.0, now, now, nil))
	mock.ExpectQuery(httpSumUsageRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(90))
	mock.ExpectQuery(httpCountActiveRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectRollback()

	w := doJSON(t, r, "/api/irrigation/manual", `{"zone_id":1,"duration":600}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Data["reason"] != "daily_budget_exceeded" {
		t.Fatalf("expected daily_budget_exceeded, got %v", resp.Data["reason"])
	}
	if remaining := resp.Data["remaining"].(float64); remaining != 10 {
		t.Fatalf("expected remaining 10, got %v", remaining)
	}
}

// 手动灌溉成功 → 200 且进行中记录落库
func TestManualEndpoint_Allowed_200(t *testing.T) {
	mock, r, cleanup := newTestRouter(t)
	defer cleanup()
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(httpZoneSelectRe.String()).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "name", "description", "daily_water_budget", "created_at", "updated_at", "deleted_at"}).
			AddRow(1, "A区", "desc", nil, now, now, nil))
	mock.ExpectQuery(httpSumUsageRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))
	mock.ExpectQuery(httpCountActiveRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(httpInsertLogRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
	mock.ExpectCommit()

	w := doJSON(t, r, "/api/irrigation/manual", `{"zone_id":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
