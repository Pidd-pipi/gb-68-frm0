package routes

import (
	"testing"

	"github.com/gin-gonic/gin"
)

// 注册全部路由时不应因 /:id/budget、/irrigation/check 等路径冲突而 panic
func TestSetupRoutes_RegisterWithoutPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SetupRoutes panicked: %v", r)
		}
	}()

	r := gin.New()
	SetupRoutes(r)

	expected := []struct {
		method string
		path   string
	}{
		{"GET", "/api/zones/:id/budget"},
		{"PUT", "/api/zones/:id/budget"},
		{"POST", "/api/irrigation/check"},
		{"POST", "/api/irrigation/manual"},
	}

	routes := r.Routes()
	has := func(method, path string) bool {
		for _, ri := range routes {
			if ri.Method == method && ri.Path == path {
				return true
			}
		}
		return false
	}

	for _, e := range expected {
		if !has(e.method, e.path) {
			t.Errorf("route %s %s not registered", e.method, e.path)
		}
	}
}
