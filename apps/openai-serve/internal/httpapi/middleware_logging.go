package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) requestLoggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		ctx := c.Request.Context()
		status := c.Writer.Status()
		latency := time.Since(start)

		attrs := []any{
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", status,
			"latency_ms", latency.Milliseconds(),
			"client_ip", c.ClientIP(),
		}

		switch {
		case status >= http.StatusInternalServerError:
			s.log.ErrorContext(ctx, "http request failed", attrs...)
		case s.log.Enabled(ctx, slog.LevelDebug):
			s.log.DebugContext(ctx, "http request", attrs...)
		}
	}
}
