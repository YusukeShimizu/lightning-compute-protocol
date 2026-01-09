package requesterwait

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lndpeermsg"
	"go.uber.org/zap"
)

type QuoteOutcome struct {
	Quote *lcpwire.Quote
	Error *lcpwire.Error
}

type ResultOutcome struct {
	// Complete is the decoded terminal `lcp_complete`.
	Complete *lcpwire.Complete

	// ResponseBytes is the reconstructed decoded bytes from the validated response
	// stream. It is set iff Complete.Status == ok.
	ResponseBytes []byte

	Error *lcpwire.Error
}

var (
	ErrPeerIDRequired       = errors.New("peer_id is required")
	ErrAlreadyWaitingQuote  = errors.New("quote waiter already registered")
	ErrAlreadyWaitingResult = errors.New("result waiter already registered")
	ErrAlreadySubscribed    = errors.New("result stream subscriber already registered")
	ErrWaitCancelled        = errors.New("wait cancelled or deadline exceeded")
)

type key struct {
	peerID string
	jobID  lcp.JobID
}

type ResultStreamEventKind uint8

const (
	ResultStreamEventKindUnspecified ResultStreamEventKind = 0
	ResultStreamEventKindBegin       ResultStreamEventKind = 1
	ResultStreamEventKindChunk       ResultStreamEventKind = 2
)

// ResultStreamEvent describes incremental result stream events for a job.
//
// This is used by server-streaming RPCs that need to forward bytes as they
// arrive, without interpreting the bytes. Events are best-effort: they do not
// replay already-processed chunks to late subscribers.
type ResultStreamEvent struct {
	Kind ResultStreamEventKind

	// Begin metadata.
	ContentType     string
	ContentEncoding string

	// Chunk data for Kind=chunk.
	Data []byte
}

type resultStreamSub struct {
	ctx    context.Context
	cancel context.CancelFunc
	ch     chan ResultStreamEvent
}

type entry struct {
	quoteOutcome   *QuoteOutcome
	quoteDelivered bool
	quoteCh        chan QuoteOutcome

	resultOutcome   *ResultOutcome
	resultDelivered bool
	resultCh        chan ResultOutcome

	resultStreamSub *resultStreamSub

	// Response stream reassembly state.
	resultStream      *streamState
	terminalResult    *lcpwire.Complete
	terminalDelivered bool
}

type Waiter struct {
	mu      sync.Mutex
	entries map[key]*entry
	logger  *zap.SugaredLogger

	maxStreamBytes uint64
	maxCallBytes   uint64
}

const (
	defaultMaxStreamBytes   = uint64(4_194_304)
	defaultMaxCallBytes     = uint64(8_388_608)
	resultStreamEventBuffer = 16
)

type streamState struct {
	streamID        lcp.Hash32
	contentType     string
	contentEncoding string

	expectedSeq uint32
	buf         []byte

	beginTotalLen *uint64
	beginSHA256   *lcp.Hash32

	done bool
}

func New(logger *zap.SugaredLogger, localManifest *lcpwire.Manifest) *Waiter {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}

	maxStreamBytes := defaultMaxStreamBytes
	maxCallBytes := defaultMaxCallBytes
	if localManifest != nil {
		if localManifest.MaxStreamBytes != 0 {
			maxStreamBytes = localManifest.MaxStreamBytes
		}
		if localManifest.MaxCallBytes != 0 {
			maxCallBytes = localManifest.MaxCallBytes
		}
	}

	return &Waiter{
		entries:        make(map[key]*entry),
		logger:         logger.With("component", "requesterwait"),
		maxStreamBytes: maxStreamBytes,
		maxCallBytes:   maxCallBytes,
	}
}

// WaitQuoteResponse blocks until it receives lcp_quote or lcp_error for the job.
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

// WaitResult blocks until it receives lcp_complete or lcp_error for the job.
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

// SubscribeResultStream registers a subscriber for incremental result stream
// events for the given job.
//
// The returned unsubscribe function MUST be called to avoid leaking waiter
// state. Unsubscribing also cancels the subscription context so that any
// in-flight event delivery can be aborted safely.
func (w *Waiter) SubscribeResultStream(
	ctx context.Context,
	peerID string,
	jobID lcp.JobID,
) (<-chan ResultStreamEvent, func(), error) {
	if peerID == "" {
		return nil, nil, ErrPeerIDRequired
	}
	if ctx == nil {
		return nil, nil, errors.New("context is required")
	}

	subCtx, cancel := context.WithCancel(ctx)
	ch := make(chan ResultStreamEvent, resultStreamEventBuffer)

	k := key{peerID: peerID, jobID: jobID}

	w.mu.Lock()
	e := w.ensureEntryLocked(k)
	if e.resultStreamSub != nil {
		w.mu.Unlock()
		cancel()
		return nil, nil, ErrAlreadySubscribed
	}
	e.resultStreamSub = &resultStreamSub{ctx: subCtx, cancel: cancel, ch: ch}
	w.mu.Unlock()

	unsubscribe := func() {
		w.mu.Lock()
		curr, ok := w.entries[k]
		if ok && curr.resultStreamSub != nil && curr.resultStreamSub.ch == ch {
			curr.resultStreamSub.cancel()
			curr.resultStreamSub = nil
			w.maybeCleanupLocked(k, curr)
		}
		w.mu.Unlock()
	}

	return ch, unsubscribe, nil
}

// HandleInboundCustomMessage decodes inbound call-scope messages and delivers them
// to the appropriate waiter. Unknown message types are ignored.
func (w *Waiter) HandleInboundCustomMessage(
	_ context.Context,
	msg lndpeermsg.InboundCustomMessage,
) {
	switch lcpwire.MessageType(msg.MsgType) {
	case lcpwire.MessageTypeQuote:
		resp, err := lcpwire.DecodeQuote(msg.Payload)
		if err != nil {
			w.logger.Debugw("decode lcp_quote failed", "err", err)
			return
		}
		w.deliverQuote(msg.PeerPubKey, resp)
	case lcpwire.MessageTypeStreamBegin:
		begin, err := lcpwire.DecodeStreamBegin(msg.Payload)
		if err != nil {
			w.logger.Debugw("decode lcp_stream_begin failed", "err", err)
			return
		}
		w.handleStreamBegin(msg.PeerPubKey, begin)
	case lcpwire.MessageTypeStreamChunk:
		chunk, err := lcpwire.DecodeStreamChunk(msg.Payload)
		if err != nil {
			w.logger.Debugw("decode lcp_stream_chunk failed", "err", err)
			return
		}
		w.handleStreamChunk(msg.PeerPubKey, chunk)
	case lcpwire.MessageTypeStreamEnd:
		end, err := lcpwire.DecodeStreamEnd(msg.Payload)
		if err != nil {
			w.logger.Debugw("decode lcp_stream_end failed", "err", err)
			return
		}
		w.handleStreamEnd(msg.PeerPubKey, end)
	case lcpwire.MessageTypeComplete:
		res, err := lcpwire.DecodeComplete(msg.Payload)
		if err != nil {
			w.logger.Debugw("decode lcp_complete failed", "err", err)
			return
		}
		w.handleTerminalComplete(msg.PeerPubKey, res)
	case lcpwire.MessageTypeError:
		errMsg, err := lcpwire.DecodeError(msg.Payload)
		if err != nil {
			w.logger.Debugw("decode lcp_error failed", "err", err)
			return
		}
		w.deliverError(msg.PeerPubKey, errMsg)
	case lcpwire.MessageTypeManifest,
		lcpwire.MessageTypeCall,
		lcpwire.MessageTypeCancel:
		return
	default:
		// ignore other message types
		return
	}
}

func (w *Waiter) handleStreamBegin(peerID string, begin lcpwire.StreamBegin) {
	if begin.Kind != lcpwire.StreamKindResponse {
		return
	}
	if begin.ContentEncoding != "identity" {
		w.deliverProtocolError(
			peerID,
			begin.Envelope,
			lcpwire.ErrorCodeUnsupportedEncoding,
			"unsupported content_encoding",
		)
		return
	}

	if begin.TotalLen != nil {
		if *begin.TotalLen > w.maxStreamBytes || *begin.TotalLen > w.maxCallBytes {
			w.deliverProtocolError(
				peerID,
				begin.Envelope,
				lcpwire.ErrorCodePayloadTooLarge,
				"response stream exceeds local limits",
			)
			return
		}
	}

	k := key{peerID: peerID, jobID: begin.Envelope.CallID}

	var sub *resultStreamSub

	w.mu.Lock()
	e := w.ensureEntryLocked(k)
	if e.resultStream != nil && !e.resultStream.done {
		// Ignore duplicate begin; the stream is already in progress.
		w.mu.Unlock()
		return
	}

	e.resultStream = &streamState{
		streamID:        begin.StreamID,
		contentType:     begin.ContentType,
		contentEncoding: begin.ContentEncoding,
		expectedSeq:     0,
		buf:             nil,
		beginTotalLen:   begin.TotalLen,
		beginSHA256:     begin.SHA256,
		done:            false,
	}
	sub = e.resultStreamSub
	w.mu.Unlock()

	w.publishResultStreamEvent(sub, ResultStreamEvent{
		Kind:            ResultStreamEventKindBegin,
		ContentType:     begin.ContentType,
		ContentEncoding: begin.ContentEncoding,
	})
}

func (w *Waiter) handleStreamChunk(peerID string, chunk lcpwire.StreamChunk) {
	k := key{peerID: peerID, jobID: chunk.Envelope.CallID}

	var sub *resultStreamSub

	w.mu.Lock()
	e := w.ensureEntryLocked(k)
	st := e.resultStream
	if st == nil || st.done {
		w.mu.Unlock()
		return
	}
	if st.streamID != chunk.StreamID {
		// Ignore chunks for unknown streams.
		w.mu.Unlock()
		return
	}

	// Enforce ordering (duplicates are ignored).
	if chunk.Seq < st.expectedSeq {
		w.mu.Unlock()
		return
	}
	if chunk.Seq > st.expectedSeq {
		env := chunk.Envelope
		w.mu.Unlock()
		w.deliverProtocolError(peerID, env, lcpwire.ErrorCodeChunkOutOfOrder, "chunk out of order")
		return
	}

	nextLen := uint64(len(st.buf)) + uint64(len(chunk.Data))
	if nextLen > w.maxStreamBytes || nextLen > w.maxCallBytes {
		env := chunk.Envelope
		w.mu.Unlock()
		w.deliverProtocolError(peerID, env, lcpwire.ErrorCodePayloadTooLarge, "response stream exceeds local limits")
		return
	}

	st.buf = append(st.buf, chunk.Data...)
	st.expectedSeq++
	sub = e.resultStreamSub
	w.mu.Unlock()

	w.publishResultStreamEvent(sub, ResultStreamEvent{
		Kind: ResultStreamEventKindChunk,
		Data: append([]byte(nil), chunk.Data...),
	})
}

func (w *Waiter) handleStreamEnd(peerID string, end lcpwire.StreamEnd) {
	k := key{peerID: peerID, jobID: end.Envelope.CallID}

	w.mu.Lock()
	e := w.ensureEntryLocked(k)
	st := e.resultStream
	if st == nil || st.done {
		w.mu.Unlock()
		return
	}
	if st.streamID != end.StreamID {
		w.mu.Unlock()
		return
	}

	// Copy bytes for validation outside the lock.
	bufCopy := append([]byte(nil), st.buf...)
	contentType := st.contentType
	contentEncoding := st.contentEncoding
	st.done = true
	w.mu.Unlock()

	if uint64(len(bufCopy)) != end.TotalLen {
		w.deliverProtocolError(peerID, end.Envelope, lcpwire.ErrorCodeChecksumMismatch, "stream total_len mismatch")
		return
	}

	sum := sha256Sum(bufCopy)
	if sum != end.SHA256 {
		w.deliverProtocolError(peerID, end.Envelope, lcpwire.ErrorCodeChecksumMismatch, "stream sha256 mismatch")
		return
	}

	w.mu.Lock()
	// Re-check entry and attempt completion with terminal result if present.
	e = w.ensureEntryLocked(k)
	terminal := e.terminalResult
	w.mu.Unlock()

	if terminal != nil {
		w.tryDeliverComplete(peerID, *terminal, bufCopy, contentType, contentEncoding)
	}
}

func (w *Waiter) handleTerminalComplete(peerID string, res lcpwire.Complete) {
	k := key{peerID: peerID, jobID: res.Envelope.CallID}

	w.mu.Lock()
	e := w.ensureEntryLocked(k)
	if e.terminalDelivered {
		w.mu.Unlock()
		return
	}
	rCopy := cloneTerminalComplete(res)
	e.terminalResult = rCopy
	e.terminalDelivered = true

	st := e.resultStream
	var (
		streamDone bool
		streamBuf  []byte
		ct         string
		ce         string
	)
	if st != nil && st.done {
		streamDone = true
		streamBuf = append([]byte(nil), st.buf...)
		ct = st.contentType
		ce = st.contentEncoding
	}
	w.mu.Unlock()

	if res.Status != lcpwire.CompleteStatusOK {
		w.deliverComplete(peerID, res, nil)
		return
	}

	if !streamDone {
		// Wait for stream_end before delivering status=ok.
		return
	}

	w.tryDeliverComplete(peerID, res, streamBuf, ct, ce)
}

func (w *Waiter) tryDeliverComplete(
	peerID string,
	terminal lcpwire.Complete,
	responseBytes []byte,
	streamContentType string,
	streamContentEncoding string,
) {
	if terminal.OK == nil {
		w.deliverComplete(peerID, terminal, responseBytes)
		return
	}

	sum := sha256Sum(responseBytes)
	if sum != terminal.OK.ResponseHash {
		w.deliverProtocolError(peerID, terminal.Envelope, lcpwire.ErrorCodeChecksumMismatch, "response_hash mismatch")
		return
	}
	if uint64(len(responseBytes)) != terminal.OK.ResponseLen {
		w.deliverProtocolError(peerID, terminal.Envelope, lcpwire.ErrorCodeChecksumMismatch, "response_len mismatch")
		return
	}
	if streamContentType != "" && streamContentType != terminal.OK.ResponseContentType {
		w.deliverProtocolError(
			peerID,
			terminal.Envelope,
			lcpwire.ErrorCodeChecksumMismatch,
			"response_content_type mismatch",
		)
		return
	}
	if streamContentEncoding != "" && streamContentEncoding != terminal.OK.ResponseContentEncoding {
		w.deliverProtocolError(
			peerID,
			terminal.Envelope,
			lcpwire.ErrorCodeChecksumMismatch,
			"response_content_encoding mismatch",
		)
		return
	}

	w.deliverComplete(peerID, terminal, responseBytes)
}

func (w *Waiter) deliverProtocolError(
	peerID string,
	env lcpwire.CallEnvelope,
	code lcpwire.ErrorCode,
	message string,
) {
	msg := message
	w.deliverError(peerID, lcpwire.Error{
		Envelope: env,
		Code:     code,
		Message:  &msg,
	})
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

func (w *Waiter) deliverQuote(peerID string, quote lcpwire.Quote) {
	k := key{peerID: peerID, jobID: quote.Envelope.CallID}
	out := QuoteOutcome{Quote: cloneQuote(quote)}

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

func (w *Waiter) deliverComplete(peerID string, res lcpwire.Complete, responseBytes []byte) {
	k := key{peerID: peerID, jobID: res.Envelope.CallID}

	out := ResultOutcome{
		Complete:      cloneTerminalComplete(res),
		ResponseBytes: append([]byte(nil), responseBytes...),
	}

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
	k := key{peerID: peerID, jobID: errMsg.Envelope.CallID}
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
	if e.resultStreamSub != nil {
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

func cloneQuote(in lcpwire.Quote) *lcpwire.Quote {
	c := in
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

func cloneTerminalComplete(in lcpwire.Complete) *lcpwire.Complete {
	c := in
	if in.OK != nil {
		okCopy := *in.OK
		c.OK = &okCopy
	}
	if in.Message != nil {
		msg := *in.Message
		c.Message = &msg
	}
	return &c
}

func sha256Sum(b []byte) lcp.Hash32 {
	sum := sha256.Sum256(b)
	return lcp.Hash32(sum)
}

func (w *Waiter) publishResultStreamEvent(sub *resultStreamSub, ev ResultStreamEvent) {
	if sub == nil {
		return
	}

	select {
	case sub.ch <- ev:
	case <-sub.ctx.Done():
	}
}
