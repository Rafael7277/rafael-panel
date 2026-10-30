package middleware

import (
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mhsanaei/3x-ui/v2/web/observability"
)

// PerformanceMiddleware records slow requests using Gin's route template,
// strips the configured private admin path, and never logs dynamic values.
func PerformanceMiddleware(privateAdminPath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()

		route := c.FullPath()
		if route == "" {
			route = "<unmatched>"
		} else if privateAdminPath != "" && privateAdminPath != "/" {
			route = strings.TrimPrefix(route, privateAdminPath)
			if route == "" {
				route = "/"
			} else if !strings.HasPrefix(route, "/") {
				route = "/" + route
			}
		}
		details := fmt.Sprintf("method=%s route=%q status=%d response_bytes=%d", c.Request.Method, route, c.Writer.Status(), c.Writer.Size())
		observability.ReportSlow("http_request", time.Since(started), details)
	}
}
