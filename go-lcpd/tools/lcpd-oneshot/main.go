package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultServerAddr = "127.0.0.1:50051"
	defaultModel      = "gpt-5.2"
	defaultTimeout    = 30 * time.Second

	defaultChatMaxPromptBytes = 12000
	temperatureMilliScale     = 1000
)

const (
	msatPerSat = 1000

	scannerInitialBufSize = 1024
	scannerMaxBufSize     = 1024 * 1024
)

var errPromptEmpty = errors.New("prompt is empty")

type runOptions struct {
	ServerAddr       string
	PeerID           string
	Model            string
	TemperatureMilli uint32
	MaxOutputTokens  uint32
	PayInvoice       bool
	Prompt           string
	Timeout          time.Duration
	OutputJSON       bool

	Chat           bool
	SystemPrompt   string
	MaxPromptBytes int
}

type pipelineResult struct {
	PeerID   string
	Quote    *lcpdv1.Quote
	Complete *lcpdv1.Complete
}

type lcpdClient interface {
	RequestQuote(
		ctx context.Context,
		in *lcpdv1.RequestQuoteRequest,
		opts ...grpc.CallOption,
	) (*lcpdv1.RequestQuoteResponse, error)
	AcceptAndExecute(
		ctx context.Context,
		in *lcpdv1.AcceptAndExecuteRequest,
		opts ...grpc.CallOption,
	) (*lcpdv1.AcceptAndExecuteResponse, error)
}

type dialFunc func(ctx context.Context, addr string) (lcpdClient, func() error, error)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	opts, err := parseArgs(os.Args[1:], os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if opts.Chat {
		runErr := runChatWithDialer(
			context.Background(),
			opts,
			defaultDialer,
			os.Stdin,
			os.Stdout,
			os.Stderr,
		)
		if runErr != nil {
			fmt.Fprintln(os.Stderr, runErr)
			return 1
		}
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()

	res, err := runWithDialer(ctx, opts, defaultDialer)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if opts.OutputJSON {
		if writeErr := writeJSONSummary(res, os.Stdout); writeErr != nil {
			fmt.Fprintln(os.Stderr, writeErr)
			return 1
		}
		return 0
	}

	fmt.Fprint(os.Stdout, formatHumanSummary(res))
	return 0
}

//nolint:funlen // CLI parsing is linear for readability.
func parseArgs(args []string, stdin io.Reader) (runOptions, error) {
	fs := flag.NewFlagSet("lcpd-oneshot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var opts runOptions
	promptFlag := fs.String("prompt", "", "prompt text (if empty, uses args or stdin)")
	chatFlag := fs.Bool("chat", false, "start an interactive chat session (REPL)")
	systemPromptFlag := fs.String("system-prompt", "", "optional system prompt prefix (chat mode)")
	maxPromptBytesFlag := fs.Int(
		"max-prompt-bytes",
		defaultChatMaxPromptBytes,
		"max bytes of conversation prompt sent to provider (chat mode; 0 disables trimming)",
	)
	fs.StringVar(
		&opts.ServerAddr,
		"server-addr",
		defaultServerAddr,
		"gRPC server address (host:port)",
	)
	fs.StringVar(&opts.PeerID, "peer-id", "", "target peer pubkey (66-hex compressed pubkey)")
	modelFlag := fs.String(
		"model",
		"",
		fmt.Sprintf(
			"OpenAI model ID for openai.chat_completions.v1 (e.g. %s)",
			defaultModel,
		),
	)
	profileFlag := fs.String(
		"profile",
		"",
		"DEPRECATED: use -model (kept for backwards compatibility)",
	)
	temperatureMilli := fs.Uint(
		"temperature-milli",
		0,
		"temperature scaled by 1000 (e.g. 700 = 0.7)",
	)
	maxOutputTokens := fs.Uint("max-output-tokens", 0, "maximum output tokens (0 = unset)")
	fs.BoolVar(
		&opts.PayInvoice,
		"pay-invoice",
		false,
		"pay the returned invoice and wait for result",
	)
	fs.DurationVar(
		&opts.Timeout,
		"timeout",
		defaultTimeout,
		"timeout per request (or per turn in -chat mode)",
	)
	fs.BoolVar(&opts.OutputJSON, "json", false, "print JSON summary instead of human-readable text")

	if err := fs.Parse(args); err != nil {
		return runOptions{}, err
	}

	opts.Chat = *chatFlag
	opts.SystemPrompt = *systemPromptFlag
	opts.MaxPromptBytes = *maxPromptBytesFlag
	if opts.MaxPromptBytes < 0 {
		return runOptions{}, errors.New("max-prompt-bytes must be >= 0")
	}

	if opts.Chat {
		if opts.OutputJSON {
			return runOptions{}, errors.New("-json is not supported with -chat")
		}
		if !opts.PayInvoice {
			return runOptions{}, errors.New("-chat requires -pay-invoice=true")
		}
	}

	prompt, err := resolvePromptByMode(opts.Chat, *promptFlag, fs.Args(), stdin)
	if err != nil {
		return runOptions{}, err
	}
	opts.Prompt = prompt

	opts.PeerID = strings.TrimSpace(opts.PeerID)

	model := strings.TrimSpace(*modelFlag)
	profile := strings.TrimSpace(*profileFlag)
	if model != "" && profile != "" && model != profile {
		return runOptions{}, errors.New("-model and -profile must match when both are set")
	}
	if model == "" {
		model = profile
	}
	if model == "" {
		model = defaultModel
	}
	opts.Model = model

	if *temperatureMilli > uint(^uint32(0)) {
		return runOptions{}, errors.New("temperature-milli must be <= 4294967295")
	}
	if *maxOutputTokens > uint(^uint32(0)) {
		return runOptions{}, errors.New("max-output-tokens must be <= 4294967295")
	}
	opts.TemperatureMilli = uint32(*temperatureMilli)
	opts.MaxOutputTokens = uint32(*maxOutputTokens)

	if opts.PeerID == "" {
		return runOptions{}, errors.New("peer-id is required")
	}
	if opts.Timeout <= 0 {
		return runOptions{}, errors.New("timeout must be > 0")
	}
	if opts.Model == "" {
		return runOptions{}, errors.New("model is required")
	}

	return opts, nil
}

func resolvePrompt(promptFlag string, args []string, stdin io.Reader) (string, error) {
	if strings.TrimSpace(promptFlag) != "" {
		return promptFlag, nil
	}

	if len(args) > 0 {
		joined := strings.TrimSpace(strings.Join(args, " "))
		if joined != "" {
			return joined, nil
		}
	}

	if stdin != nil {
		raw, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		trimmed := strings.TrimRight(string(raw), "\r\n")
		if strings.TrimSpace(trimmed) != "" {
			return trimmed, nil
		}
	}

	return "", errPromptEmpty
}

func resolvePromptByMode(
	chat bool,
	promptFlag string,
	args []string,
	stdin io.Reader,
) (string, error) {
	if !chat {
		return resolvePrompt(promptFlag, args, stdin)
	}

	prompt, err := resolvePrompt(promptFlag, args, nil)
	if err == nil {
		return prompt, nil
	}
	if errors.Is(err, errPromptEmpty) {
		return "", nil
	}
	return "", err
}

func runWithDialer(
	ctx context.Context,
	opts runOptions,
	dial dialFunc,
) (pipelineResult, error) {
	client, closeFn, err := dial(ctx, opts.ServerAddr)
	if err != nil {
		return pipelineResult{}, err
	}
	defer func() { _ = closeFn() }()

	return runPipeline(ctx, client, opts)
}

func defaultDialer(_ context.Context, addr string) (lcpdClient, func() error, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("create gRPC client %s: %w", addr, err)
	}

	client := lcpdv1.NewLCPDServiceClient(conn)
	return client, conn.Close, nil
}

func runChatWithDialer(
	ctx context.Context,
	opts runOptions,
	dial dialFunc,
	in io.Reader,
	out io.Writer,
	errOut io.Writer,
) error {
	client, closeFn, err := dial(ctx, opts.ServerAddr)
	if err != nil {
		return err
	}
	defer func() { _ = closeFn() }()

	session := newChatSession(opts)

	inFile, inOK := in.(*os.File)
	outFile, outOK := out.(*os.File)
	if inOK && outOK &&
		term.IsTerminal(int(inFile.Fd())) &&
		term.IsTerminal(int(outFile.Fd())) {
		tuiErr := runChatTUI(ctx, session, client, inFile, outFile)
		if tuiErr == nil {
			return nil
		}
		fmt.Fprintf(errOut, "tui disabled (%v); falling back to plain REPL\n", tuiErr)
	}

	return session.Run(ctx, client, in, out, errOut)
}

func runPipeline(
	ctx context.Context,
	client lcpdClient,
	opts runOptions,
) (pipelineResult, error) {
	requestJSON, err := buildOpenAIChatCompletionsV1RequestJSON(
		opts.Model,
		opts.Prompt,
		opts.TemperatureMilli,
		opts.MaxOutputTokens,
	)
	if err != nil {
		return pipelineResult{}, err
	}

	paramsBytes, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(
		lcpwire.OpenAIChatCompletionsV1Params{Model: opts.Model},
	)
	if err != nil {
		return pipelineResult{}, fmt.Errorf("encode openai.chat_completions.v1 params: %w", err)
	}

	quoteResp, err := client.RequestQuote(ctx, &lcpdv1.RequestQuoteRequest{
		PeerId: opts.PeerID,
		Call: &lcpdv1.CallSpec{
			Method:                 "openai.chat_completions.v1",
			Params:                 paramsBytes,
			RequestBytes:           requestJSON,
			RequestContentType:     "application/json; charset=utf-8",
			RequestContentEncoding: "identity",
		},
	})
	if err != nil {
		return pipelineResult{}, fmt.Errorf("request quote: %w", err)
	}

	quote := quoteResp.GetQuote()
	if quote == nil {
		return pipelineResult{}, errors.New("request quote: response quote is nil")
	}

	res := pipelineResult{
		PeerID:   opts.PeerID,
		Quote:    quote,
		Complete: nil,
	}

	if !opts.PayInvoice {
		return res, nil
	}

	execResp, err := client.AcceptAndExecute(ctx, &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     opts.PeerID,
		CallId:     quote.GetCallId(),
		PayInvoice: true,
	})
	if err != nil {
		return pipelineResult{}, fmt.Errorf("accept and execute: %w", err)
	}
	if execResp == nil {
		return pipelineResult{}, errors.New("accept and execute: response is nil")
	}
	if execResp.GetComplete() == nil {
		return pipelineResult{}, errors.New("accept and execute: complete is nil")
	}

	res.Complete = execResp.GetComplete()
	return res, nil
}

type openAIChatCompletionsV1Request struct {
	Model string `json:"model"`

	Messages []openAIChatMessage `json:"messages"`

	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *uint32  `json:"max_tokens,omitempty"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func buildOpenAIChatCompletionsV1RequestJSON(
	model string,
	prompt string,
	temperatureMilli uint32,
	maxOutputTokens uint32,
) ([]byte, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, errors.New("model is required")
	}

	req := openAIChatCompletionsV1Request{
		Model: model,
		Messages: []openAIChatMessage{
			{Role: "user", Content: prompt},
		},
	}
	if temperatureMilli != 0 {
		temp := float64(temperatureMilli) / temperatureMilliScale
		req.Temperature = &temp
	}
	if maxOutputTokens != 0 {
		maxTokens := maxOutputTokens
		req.MaxTokens = &maxTokens
	}

	out, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal OpenAI chat completions request: %w", err)
	}
	return out, nil
}

func formatHumanSummary(res pipelineResult) string {
	var b strings.Builder

	if res.Quote == nil {
		fmt.Fprintf(&b, "peer_id=%s\n", res.PeerID)
		fmt.Fprintf(&b, "quote:\n(none)\n")
		return b.String()
	}

	fmt.Fprintf(&b, "peer_id=%s protocol_version=%d\n", res.PeerID, res.Quote.GetProtocolVersion())
	fmt.Fprintf(
		&b,
		"price_sat=%s price_msat=%d\n",
		formatMsatAsSat(res.Quote.GetPriceMsat()),
		res.Quote.GetPriceMsat(),
	)
	fmt.Fprintf(
		&b,
		"terms_hash=%s call_id=%s\n",
		hex.EncodeToString(res.Quote.GetTermsHash()),
		hex.EncodeToString(res.Quote.GetCallId()),
	)
	fmt.Fprintf(&b, "payment_request:\n%s\n", res.Quote.GetPaymentRequest())

	if res.Complete == nil {
		fmt.Fprintf(&b, "complete:\n(none)\n")
		return b.String()
	}

	fmt.Fprintf(&b, "status=%s\n", res.Complete.GetStatus().String())
	if msg := strings.TrimSpace(res.Complete.GetMessage()); msg != "" {
		fmt.Fprintf(&b, "message=%s\n", msg)
	}
	if ct := strings.TrimSpace(res.Complete.GetResponseContentType()); ct != "" {
		fmt.Fprintf(&b, "content_type=%s\n", ct)
	}
	fmt.Fprintf(&b, "response:\n%s\n", string(res.Complete.GetResponseBytes()))

	return b.String()
}

func writeJSONSummary(res pipelineResult, w io.Writer) error {
	payload := struct {
		PeerID          string `json:"peer_id"`
		ProtocolVersion uint32 `json:"protocol_version"`
		PriceMsat       uint64 `json:"price_msat"`
		PriceSat        string `json:"price_sat"`
		TermsHashHex    string `json:"terms_hash_hex"`
		CallIDHex       string `json:"call_id_hex"`
		PaymentRequest  string `json:"payment_request"`
		Status          string `json:"status"`
		Message         string `json:"message,omitempty"`
		ResponseText    string `json:"response_text"`
		ContentType     string `json:"content_type"`
	}{
		PeerID:       res.PeerID,
		Status:       "",
		Message:      "",
		ResponseText: "",
		ContentType:  "",
	}

	if res.Quote != nil {
		payload.ProtocolVersion = res.Quote.GetProtocolVersion()
		payload.PriceMsat = res.Quote.GetPriceMsat()
		payload.PriceSat = formatMsatAsSat(res.Quote.GetPriceMsat())
		payload.TermsHashHex = hex.EncodeToString(res.Quote.GetTermsHash())
		payload.CallIDHex = hex.EncodeToString(res.Quote.GetCallId())
		payload.PaymentRequest = res.Quote.GetPaymentRequest()
	}

	if res.Complete != nil {
		payload.Status = res.Complete.GetStatus().String()
		payload.Message = res.Complete.GetMessage()
		payload.ResponseText = string(res.Complete.GetResponseBytes())
		payload.ContentType = res.Complete.GetResponseContentType()
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func formatMsatAsSat(msat uint64) string {
	sat := msat / msatPerSat
	rem := msat % msatPerSat
	if rem == 0 {
		return strconv.FormatUint(sat, 10)
	}
	return fmt.Sprintf("%d.%03d", sat, rem)
}

// readLine is a tiny wrapper around bufio.Scanner so we can keep scanning concerns
// out of the chat session code.
type readLine struct {
	scanner *bufio.Scanner
}

func newReadLine(r io.Reader) *readLine {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, scannerInitialBufSize), scannerMaxBufSize)
	return &readLine{scanner: s}
}

func (r *readLine) Scan() (string, bool, error) {
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	return r.scanner.Text(), true, nil
}
