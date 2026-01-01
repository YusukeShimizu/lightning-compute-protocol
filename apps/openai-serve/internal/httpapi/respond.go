package httpapi

import (
	"errors"
	"net/http"

	"github.com/bruwbird/lcp/apps/openai-serve/internal/openai"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func writeJSON(c *gin.Context, status int, v any) {
	c.JSON(status, v)
}

func writeOpenAIError(c *gin.Context, status int, typ, message string) {
	writeJSON(c, status, openai.ErrorResponse{
		Error: openai.ErrorDetail{
			Message: message,
			Type:    typ,
			Param:   nil,
			Code:    nil,
		},
	})
}

func httpStatusFromGRPC(err error) int {
	if err == nil {
		return http.StatusOK
	}
	s, ok := status.FromError(err)
	if !ok {
		return http.StatusInternalServerError
	}
	switch s.Code() {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return http.StatusRequestTimeout
	case codes.Unknown:
		return http.StatusInternalServerError
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusBadRequest
	case codes.AlreadyExists:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.FailedPrecondition:
		return http.StatusBadRequest
	case codes.Aborted:
		return http.StatusConflict
	case codes.OutOfRange:
		return http.StatusBadRequest
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Internal:
		return http.StatusInternalServerError
	case codes.DataLoss:
		return http.StatusInternalServerError
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.Unavailable:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func grpcErrMessage(err error) string {
	if err == nil {
		return ""
	}
	s, ok := status.FromError(err)
	if !ok {
		return err.Error()
	}
	if msg := s.Message(); msg != "" {
		return msg
	}
	return err.Error()
}

type requestBodyTooLargeError struct{}

func (requestBodyTooLargeError) Error() string { return "request body too large" }

func isRequestBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return true
	}
	var tooLargeErr requestBodyTooLargeError
	return errors.As(err, &tooLargeErr)
}
