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

	terminalComplete *lcpdv1.Complete
}

func (s *Server) acceptAndExecuteStream(
	c *gin.Context,
	peerID string,
	callID []byte,
) (lcpdv1.LCPDService_AcceptAndExecuteStreamClient, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.TimeoutExecute)
	stream, err := s.lcpd.AcceptAndExecuteStream(ctx, &lcpdv1.AcceptAndExecuteStreamRequest{
		PeerId:     peerID,
		CallId:     callID,
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
	out := newStreamWriteResult()
	for {
		msg, err := stream.Recv()
		if err != nil {
			return finishStreamWrite(out, err)
		}

		done, handleErr := s.handleStreamMessage(c, &out, msg)
		if handleErr != nil {
			return out, handleErr
		}
		if done {
			return out, nil
		}
	}
}

func newStreamWriteResult() streamWriteResult {
	return streamWriteResult{
		contentType:     defaultSSEContentType,
		contentEncoding: contentEncodingIdentity,
	}
}

func finishStreamWrite(out streamWriteResult, err error) (streamWriteResult, error) {
	if errors.Is(err, io.EOF) {
		if out.terminalComplete == nil {
			return out, errors.New("stream ended without a terminal complete")
		}
		return out, nil
	}
	return out, err
}

func (s *Server) handleStreamMessage(
	c *gin.Context,
	out *streamWriteResult,
	msg *lcpdv1.AcceptAndExecuteStreamResponse,
) (bool, error) {
	switch ev := msg.GetEvent().(type) {
	case *lcpdv1.AcceptAndExecuteStreamResponse_ResponseBegin:
		applyStreamBegin(out, ev.ResponseBegin)
		return false, nil
	case *lcpdv1.AcceptAndExecuteStreamResponse_ResponseChunk:
		return false, writeStreamChunk(c, out, ev.ResponseChunk)
	case *lcpdv1.AcceptAndExecuteStreamResponse_ResponseEnd:
		// Metadata only (hash/len). Ignored by the HTTP passthrough.
		return false, nil
	case *lcpdv1.AcceptAndExecuteStreamResponse_Complete:
		out.terminalComplete = ev.Complete
		return true, nil
	default:
		return false, nil
	}
}

func applyStreamBegin(out *streamWriteResult, begin *lcpdv1.ResponseStreamBegin) {
	if begin == nil {
		return
	}
	if ct := strings.TrimSpace(begin.GetContentType()); ct != "" {
		out.contentType = ct
	}
	if ce := strings.TrimSpace(begin.GetContentEncoding()); ce != "" {
		out.contentEncoding = ce
	}
}

func writeStreamChunk(c *gin.Context, out *streamWriteResult, chunk *lcpdv1.ResponseStreamChunk) error {
	if chunk == nil {
		return nil
	}

	data := chunk.GetData()
	if len(data) == 0 {
		return nil
	}

	if err := ensureStreamWriteReady(c, out); err != nil {
		return err
	}

	n, writeErr := c.Writer.Write(data)
	out.bytesWritten += n
	out.wroteBody = true
	flushHTTPWriter(c.Writer)
	return writeErr
}

func ensureStreamWriteReady(c *gin.Context, out *streamWriteResult) error {
	if out.wroteBody {
		return nil
	}

	if enc := strings.TrimSpace(out.contentEncoding); enc != "" && enc != contentEncodingIdentity {
		return fmt.Errorf("unsupported content encoding: %q", enc)
	}

	writeSSEHeaders(c, out.contentType)
	c.Status(http.StatusOK)
	return nil
}

func flushHTTPWriter(w io.Writer) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
