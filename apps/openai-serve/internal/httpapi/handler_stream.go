package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/gin-gonic/gin"
)

const defaultSSEContentType = "text/event-stream; charset=utf-8"

type streamWriteResult struct {
	bytesWritten int
	wroteBody    bool

	contentType     string
	contentEncoding string

	terminalResult *lcpdv1.Result
}

func (s *Server) acceptAndExecuteStream(
	c *gin.Context,
	peerID string,
	jobID []byte,
) (lcpdv1.LCPDService_AcceptAndExecuteStreamClient, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.TimeoutExecute)
	stream, err := s.lcpd.AcceptAndExecuteStream(ctx, &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		JobId:      jobID,
		PayInvoice: true,
	})
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	return stream, cancel, nil
}

func writeSSEHeaders(c *gin.Context, contentType string) {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = defaultSSEContentType
	}

	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
}

func (s *Server) writeLCPStreamToHTTP(
	c *gin.Context,
	stream lcpdv1.LCPDService_AcceptAndExecuteStreamClient,
) (streamWriteResult, error) {
	var out streamWriteResult
	out.contentType = defaultSSEContentType
	out.contentEncoding = contentEncodingIdentity

	flush := func() {
		if f, ok := c.Writer.(http.Flusher); ok {
			f.Flush()
		}
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if out.terminalResult == nil {
					return out, errors.New("stream ended without a terminal result")
				}
				return out, nil
			}
			return out, err
		}

		switch ev := msg.GetEvent().(type) {
		case *lcpdv1.AcceptAndExecuteStreamResponse_ResultBegin:
			begin := ev.ResultBegin
			if begin == nil {
				continue
			}
			if ct := strings.TrimSpace(begin.GetContentType()); ct != "" {
				out.contentType = ct
			}
			if ce := strings.TrimSpace(begin.GetContentEncoding()); ce != "" {
				out.contentEncoding = ce
			}
		case *lcpdv1.AcceptAndExecuteStreamResponse_ResultChunk:
			chunk := ev.ResultChunk
			if chunk == nil {
				continue
			}
			data := chunk.GetData()
			if len(data) == 0 {
				continue
			}

			if !out.wroteBody {
				if enc := strings.TrimSpace(out.contentEncoding); enc != "" && enc != contentEncodingIdentity {
					return out, fmt.Errorf("unsupported content encoding: %q", enc)
				}

				writeSSEHeaders(c, out.contentType)
				c.Status(http.StatusOK)
			}

			n, writeErr := c.Writer.Write(data)
			out.bytesWritten += n
			out.wroteBody = true
			flush()
			if writeErr != nil {
				return out, writeErr
			}
		case *lcpdv1.AcceptAndExecuteStreamResponse_Result:
			out.terminalResult = ev.Result
			return out, nil
		case *lcpdv1.AcceptAndExecuteStreamResponse_ResultEnd:
			// Metadata only (hash/len). Ignored by the HTTP passthrough.
			continue
		default:
			continue
		}
	}
}

