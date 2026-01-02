package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleHealthz(c *gin.Context) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.Status(http.StatusMethodNotAllowed)
		return
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, "ok\n")
}
