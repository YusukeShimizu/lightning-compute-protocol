package requesterwait

import (
	"context"
	"errors"
	"sync"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lndpeermsg"
	"go.uber.org/zap"
)

type QuoteOutcome struct {
	QuoteResponse *lcpwire.QuoteResponse
	Error         *lcpwire.Error
}

type ResultOutcome struct {
	Result *lcpwire.Result
	Error  *lcpwire.Error
}

var (
	ErrPeerIDRequired       = errors.New("peer_id is required")
	ErrAlreadyWaitingQuote  = errors.New("quote waiter already registered")
	ErrAlreadyWaitingResult = errors.New("result waiter already registered")
	ErrWaitCancelled        = errors.New("wait cancelled or deadline exceeded")
)

type key struct {
	peerID string
	jobID  lcp.JobID
}

type entry struct {
	quoteOutcome   *QuoteOutcome
	quoteDelivered bool
	quoteCh        chan QuoteOutcome

	resultOutcome   *ResultOutcome
	resultDelivered bool
	resultCh        chan ResultOutcome
}

type Waiter struct {
	mu      sync.Mutex
	entries map[key]*entry
	logger  *zap.SugaredLogger
}

func New(logger *zap.SugaredLogger) *Waiter {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &Waiter{
		entries: make(map[key]*entry),
		logger:  logger.With("component", "requesterwait"),
	}
}

// WaitQuoteResponse blocks until it receives lcp_quote_response or lcp_error for the job.
func (w *Waiter) WaitQuoteResponse(
	ctx context.Context,
	peerID string,
	jobID lcp.JobID,
) (QuoteOutcome, error) {
	return waitOutcome(
		ctx,
		w,
		peerID,
		jobID,
		func(e *entry) *QuoteOutcome { return e.quoteOutcome },
		func(e *entry) chan QuoteOutcome { return e.quoteCh },
		func(e *entry, ch chan QuoteOutcome) { e.quoteCh = ch },
		func(e *entry) { e.quoteDelivered = true },
		ErrAlreadyWaitingQuote,
	)
}

// WaitResult blocks until it receives lcp_result or lcp_error for the job.
func (w *Waiter) WaitResult(
	ctx context.Context,
	peerID string,
	jobID lcp.JobID,
) (ResultOutcome, error) {
	return waitOutcome(
		ctx,
		w,
		peerID,
		jobID,
		func(e *entry) *ResultOutcome { return e.resultOutcome },
		func(e *entry) chan ResultOutcome { return e.resultCh },
		func(e *entry, ch chan ResultOutcome) { e.resultCh = ch },
		func(e *entry) { e.resultDelivered = true },
		ErrAlreadyWaitingResult,
	)
}

// HandleInboundCustomMessage decodes inbound job-scope messages and delivers them
// to the appropriate waiter. Unknown message types are ignored.
func (w *Waiter) HandleInboundCustomMessage(
	_ context.Context,
	msg lndpeermsg.InboundCustomMessage,
) {
	switch lcpwire.MessageType(msg.MsgType) {
	case lcpwire.MessageTypeQuoteResponse:
		resp, err := lcpwire.DecodeQuoteResponse(msg.Payload)
		if err != nil {
			w.logger.Debugw("decode lcp_quote_response failed", "err", err)
			return
		}
		w.deliverQuoteResponse(msg.PeerPubKey, resp)
	case lcpwire.MessageTypeResult:
		res, err := lcpwire.DecodeResult(msg.Payload)
		if err != nil {
			w.logger.Debugw("decode lcp_result failed", "err", err)
			return
		}
		w.deliverResult(msg.PeerPubKey, res)
	case lcpwire.MessageTypeError:
		errMsg, err := lcpwire.DecodeError(msg.Payload)
		if err != nil {
			w.logger.Debugw("decode lcp_error failed", "err", err)
			return
		}
		w.deliverError(msg.PeerPubKey, errMsg)
	case lcpwire.MessageTypeManifest,
		lcpwire.MessageTypeQuoteRequest,
		lcpwire.MessageTypeCancel:
		return
	default:
		// ignore other message types
		return
	}
}

func waitOutcome[T any](
	ctx context.Context,
	w *Waiter,
	peerID string,
	jobID lcp.JobID,
	getOutcome func(*entry) *T,
	getCh func(*entry) chan T,
	setCh func(*entry, chan T),
	markDelivered func(*entry),
	alreadyWaitingErr error,
) (T, error) {
	var zero T
	if peerID == "" {
		return zero, ErrPeerIDRequired
	}

	k := key{peerID: peerID, jobID: jobID}

	w.mu.Lock()
	e := w.ensureEntryLocked(k)

	if outPtr := getOutcome(e); outPtr != nil {
		out := *outPtr
		markDelivered(e)
		w.maybeCleanupLocked(k, e)
		w.mu.Unlock()
		return out, nil
	}

	if existingCh := getCh(e); existingCh != nil {
		w.mu.Unlock()
		return zero, alreadyWaitingErr
	}

	ch := make(chan T, 1)
	setCh(e, ch)
	w.mu.Unlock()

	select {
	case out := <-ch:
		w.mu.Lock()
		if curr, ok := w.entries[k]; ok {
			markDelivered(curr)
			w.maybeCleanupLocked(k, curr)
		}
		w.mu.Unlock()
		return out, nil
	case <-ctx.Done():
		w.mu.Lock()
		if curr, ok := w.entries[k]; ok && getCh(curr) == ch {
			setCh(curr, nil)
			w.maybeCleanupLocked(k, curr)
		}
		w.mu.Unlock()
		return zero, ErrWaitCancelled
	}
}

func (w *Waiter) ensureEntryLocked(k key) *entry {
	e, ok := w.entries[k]
	if ok {
		return e
	}
	e = &entry{}
	w.entries[k] = e
	return e
}

func (w *Waiter) deliverQuoteResponse(peerID string, resp lcpwire.QuoteResponse) {
	k := key{peerID: peerID, jobID: resp.Envelope.JobID}
	out := QuoteOutcome{QuoteResponse: cloneQuoteResponse(resp)}

	var ch chan QuoteOutcome

	w.mu.Lock()
	e := w.ensureEntryLocked(k)
	e.quoteOutcome = &out
	ch = e.quoteCh
	e.quoteCh = nil
	w.maybeCleanupLocked(k, e)
	w.mu.Unlock()

	if ch != nil {
		ch <- out
	}
}

func (w *Waiter) deliverResult(peerID string, res lcpwire.Result) {
	k := key{peerID: peerID, jobID: res.Envelope.JobID}
	out := ResultOutcome{Result: cloneResult(res)}

	var ch chan ResultOutcome

	w.mu.Lock()
	e := w.ensureEntryLocked(k)
	e.resultOutcome = &out
	ch = e.resultCh
	e.resultCh = nil
	w.maybeCleanupLocked(k, e)
	w.mu.Unlock()

	if ch != nil {
		ch <- out
	}
}

func (w *Waiter) deliverError(peerID string, errMsg lcpwire.Error) {
	k := key{peerID: peerID, jobID: errMsg.Envelope.JobID}
	out := QuoteOutcome{Error: cloneError(errMsg)}
	outResult := ResultOutcome{Error: cloneError(errMsg)}

	var quoteCh chan QuoteOutcome
	var resultCh chan ResultOutcome

	w.mu.Lock()
	e := w.ensureEntryLocked(k)

	e.quoteOutcome = &out
	quoteCh = e.quoteCh
	e.quoteCh = nil

	e.resultOutcome = &outResult
	resultCh = e.resultCh
	e.resultCh = nil

	w.maybeCleanupLocked(k, e)
	w.mu.Unlock()

	if quoteCh != nil {
		quoteCh <- out
	}
	if resultCh != nil {
		resultCh <- outResult
	}
}

func (w *Waiter) maybeCleanupLocked(k key, e *entry) {
	if e == nil {
		return
	}
	if e.quoteCh != nil || e.resultCh != nil {
		return
	}
	if !e.quoteDelivered || !e.resultDelivered {
		return
	}
	delete(w.entries, k)
}

func cloneQuoteResponse(in lcpwire.QuoteResponse) *lcpwire.QuoteResponse {
	c := in
	return &c
}

func cloneResult(in lcpwire.Result) *lcpwire.Result {
	c := in
	if len(in.Result) > 0 {
		c.Result = append([]byte(nil), in.Result...)
	}
	if in.ContentType != nil {
		ct := *in.ContentType
		c.ContentType = &ct
	}
	return &c
}

func cloneError(in lcpwire.Error) *lcpwire.Error {
	c := in
	if in.Message != nil {
		msg := *in.Message
		c.Message = &msg
	}
	return &c
}

var _ lndpeermsg.InboundMessageHandler = (*Waiter)(nil)
