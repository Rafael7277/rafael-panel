package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// RedirectMiddleware canonicalizes legacy panel URLs only inside the
// configured panel path. When a custom path is configured, requests outside
// that path must not be redirected to it because that would disclose it.
func RedirectMiddleware(basePath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "" {
			path = "/"
		}

		prefix := ""
		if basePath != "/" {
			if !strings.HasPrefix(path, basePath) {
				c.Next()
				return
			}
			prefix = strings.TrimRight(basePath, "/")
			path = "/" + strings.TrimPrefix(path, basePath)
		}

		redirects := map[string]string{
			"/panel/API": "/panel/api",
			"/xui/API":   "/panel/api",
			"/xui":       "/panel",
		}

		for from, to := range redirects {
			if strings.HasPrefix(path, from) {
				target := prefix + to + path[len(from):]
				c.Redirect(http.StatusPermanentRedirect, withQuery(target, c.Request.URL.RawQuery))
				c.Abort()
				return
			}
		}
		c.Next()
	}
}

func withQuery(path string, rawQuery string) string {
	if rawQuery == "" {
		return path
	}
	return path + "?" + rawQuery
}
