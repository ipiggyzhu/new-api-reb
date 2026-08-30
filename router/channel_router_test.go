package router

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelStatusRoutesUseOperatePermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodPost, "/:id/status", authz.ChannelOperate, controller.UpdateChannelStatus)
	assertChannelRoutePermission(t, http.MethodPost, "/status/batch", authz.ChannelOperate, controller.BatchUpdateChannelStatus)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.UpdateChannel)
}

func TestChannelDeleteRoutesUseSensitiveWritePermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodDelete, "/:id", authz.ChannelSensitiveWrite, controller.DeleteChannel)
	assertChannelRoutePermission(t, http.MethodPost, "/batch", authz.ChannelSensitiveWrite, controller.DeleteChannelBatch)
	assertChannelRoutePermission(t, http.MethodDelete, "/disabled", authz.ChannelSensitiveWrite, controller.DeleteDisabledChannel)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.UpdateChannel)
	assertChannelRoutePermission(t, http.MethodPut, "/tag", authz.ChannelWrite, controller.EditTagChannels)
	assertChannelRoutePermission(t, http.MethodPost, "/batch/tag", authz.ChannelWrite, controller.BatchSetChannelTag)
}

func TestChannelStatusRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api")

	require.NotPanics(t, func() {
		registerChannelRoutes(api)
	})
}

// TestChannelReadRoutesAreRegistered guards against a handler that exists but was
// never wired up. Nothing else catches that: the handlers are exported, so an
// unreferenced one still compiles and still passes vet, and an unauthenticated
// request cannot tell the difference because AdminAuth runs before routing and
// answers 401 for unknown paths too.
func TestChannelReadRoutesAreRegistered(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodGet, "/dynamic_score", authz.ChannelRead, controller.GetChannelDynamicScore)
	assertChannelRoutePermission(t, http.MethodGet, "/in_flight", authz.ChannelRead, controller.GetChannelInFlight)
}

// TestStaticChannelRoutesAreNotShadowedByIDParam pins the dispatch of the static
// GET paths that sit alongside "/:id". Registering one of them after "/:id" is the
// easy mistake, and it would send every request for it to GetChannel with
// id="dynamic_score" instead — a 200 with the wrong body, not an error.
func TestStaticChannelRoutesAreNotShadowedByIDParam(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, path := range []string{"/dynamic_score", "/in_flight", "/models", "/search", "/ops"} {
		t.Run(path, func(t *testing.T) {
			engine := gin.New()
			group := engine.Group("/api/channel")

			// Replay the real table's order with sentinel handlers so the assertion is
			// about routing alone, with no auth, database or controller involved.
			for _, route := range channelPermissionRoutes {
				matched := route.path
				group.Handle(route.method, route.path, func(c *gin.Context) {
					c.String(http.StatusOK, matched)
				})
			}

			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/channel"+path, nil))

			require.Equal(t, http.StatusOK, recorder.Code)
			assert.Equal(t, path, recorder.Body.String(),
				"GET %s was dispatched to %q; a static path must win over /:id", path, recorder.Body.String())
		})
	}
}

func assertChannelRoutePermission(t *testing.T, method string, path string, permission authz.Permission, handler any) {
	t.Helper()
	for _, route := range channelPermissionRoutes {
		if route.method == method && route.path == path {
			assert.Equal(t, permission, route.permission)
			assert.Equal(t, reflect.ValueOf(handler).Pointer(), reflect.ValueOf(route.handler).Pointer())
			return
		}
	}
	t.Fatalf("route %s %s not found", method, path)
}
