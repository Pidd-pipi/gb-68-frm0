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
	"gorm.io/gorm"

	"irrigation/pkg/database"
)

var (
	zoneGetNoLockRe = regexp.MustCompile(`(?is)SELECT .*"irrigation_zones".*ORDER BY .*LIMIT`)
	zoneUpdateRe    = regexp.MustCompile(`(?is)UPDATE "irrigation_zones" SET`)
)

func newZoneRouter(t *testing.T) (sqlmock.Sqlmock, *gin.Engine, func()) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}

	gdb, err := gormOpenTestDB(t, sqlDB)
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	old := database.DB
	database.DB = gdb

	gin.SetMode(gin.TestMode)
	r := gin.New()
	zc := NewZoneController()
	r.GET("/api/zones/:id/budget", zc.GetBudget)
	r.PUT("/api/zones/:id/budget", zc.UpdateBudget)

	return mock, r, func() {
		database.DB = old
		_ = sqlDB.Close()
	}
}

func zoneRow(budget interface{}) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows(
		[]string{"id", "name", "description", "daily_water_budget", "created_at", "updated_at", "deleted_at"}).
		AddRow(1, "A区", "desc", budget, now, now, nil)
}

func TestGetBudget_Set_ReturnsRemaining(t *testing.T) {
	mock, r, cleanup := newZoneRouter(t)
	defer cleanup()
	mock.ExpectQuery(zoneGetNoLockRe.String()).WillReturnRows(zoneRow(500.0))
	mock.ExpectQuery(httpSumUsageRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(120))

	req := httptest.NewRequest(http.MethodGet, "/api/zones/1/budget", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Data["daily_budget"].(float64) != 500 || resp.Data["used_today"].(float64) != 120 ||
		resp.Data["remaining"].(float64) != 380 {
		t.Fatalf("unexpected payload: %+v", resp.Data)
	}
}

func TestGetBudget_Unlimited_Nulls(t *testing.T) {
	mock, r, cleanup := newZoneRouter(t)
	defer cleanup()
	mock.ExpectQuery(zoneGetNoLockRe.String()).WillReturnRows(zoneRow(nil))
	mock.ExpectQuery(httpSumUsageRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))

	req := httptest.NewRequest(http.MethodGet, "/api/zones/1/budget", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Data["daily_budget"] != nil || resp.Data["remaining"] != nil {
		t.Fatalf("expected null budget/remaining, got %+v", resp.Data)
	}
}

func TestGetBudget_ZoneNotFound_404(t *testing.T) {
	mock, r, cleanup := newZoneRouter(t)
	defer cleanup()
	mock.ExpectQuery(zoneGetNoLockRe.String()).
		WillReturnError(gorm.ErrRecordNotFound)

	req := httptest.NewRequest(http.MethodGet, "/api/zones/99/budget", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestPutBudget_Set_200(t *testing.T) {
	mock, r, cleanup := newZoneRouter(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec(zoneUpdateRe.String()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(zoneGetNoLockRe.String()).WillReturnRows(zoneRow(500.0))
	mock.ExpectQuery(httpSumUsageRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))

	req := httptest.NewRequest(http.MethodPut, "/api/zones/1/budget",
		bytes.NewBufferString(`{"daily_water_budget":500}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPutBudget_ClearWithNull_200(t *testing.T) {
	mock, r, cleanup := newZoneRouter(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec(zoneUpdateRe.String()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(zoneGetNoLockRe.String()).WillReturnRows(zoneRow(nil))
	mock.ExpectQuery(httpSumUsageRe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))

	req := httptest.NewRequest(http.MethodPut, "/api/zones/1/budget",
		bytes.NewBufferString(`{"daily_water_budget":null}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Data["daily_budget"] != nil {
		t.Fatalf("expected cleared budget null, got %v", resp.Data["daily_budget"])
	}
}

func TestPutBudget_MissingField_400(t *testing.T) {
	_, r, cleanup := newZoneRouter(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPut, "/api/zones/1/budget",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestPutBudget_Negative_400(t *testing.T) {
	_, r, cleanup := newZoneRouter(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPut, "/api/zones/1/budget",
		bytes.NewBufferString(`{"daily_water_budget":-1}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
