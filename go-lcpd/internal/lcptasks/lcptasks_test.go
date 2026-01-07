package lcptasks_test

import (
	"errors"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcptasks"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/google/go-cmp/cmp"
)

func TestValidateTask_OpenAIChatCompletionsV1(t *testing.T) {
	t.Parallel()

	requestJSON := []byte(`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}]}`)

	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
			OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
				RequestJson: requestJSON,
				Params: &lcpdv1.OpenAIChatCompletionsV1Params{
					Model: "gpt-5.2",
				},
			},
		},
	}

	if err := lcptasks.ValidateTask(task); err != nil {
		t.Fatalf("ValidateTask: %v", err)
	}
}

func TestValidateTask_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		task    *lcpdv1.Task
		wantErr error
	}{
		{
			name:    "nil task",
			task:    nil,
			wantErr: lcptasks.ErrTaskRequired,
		},
		{
			name:    "missing spec",
			task:    &lcpdv1.Task{},
			wantErr: lcptasks.ErrTaskSpecRequired,
		},
		{
			name: "openai_chat_completions_v1 spec nil",
			task: &lcpdv1.Task{
				Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{},
			},
			wantErr: lcptasks.ErrOpenAIChatCompletionsV1TaskRequired,
		},
		{
			name: "openai_chat_completions_v1 request_json empty",
			task: &lcpdv1.Task{
				Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
					OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
						Params: &lcpdv1.OpenAIChatCompletionsV1Params{Model: "gpt-5.2"},
					},
				},
			},
			wantErr: lcptasks.ErrOpenAIChatCompletionsV1RequestJSONRequired,
		},
		{
			name: "openai_chat_completions_v1 params nil",
			task: &lcpdv1.Task{
				Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
					OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
						RequestJson: []byte(`{"model":"gpt-5.2"}`),
					},
				},
			},
			wantErr: lcptasks.ErrOpenAIChatCompletionsV1ParamsRequired,
		},
		{
			name: "openai_chat_completions_v1 params.model empty",
			task: &lcpdv1.Task{
				Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
					OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
						RequestJson: []byte(`{"model":"gpt-5.2"}`),
						Params:      &lcpdv1.OpenAIChatCompletionsV1Params{},
					},
				},
			},
			wantErr: lcptasks.ErrOpenAIChatCompletionsV1ModelRequired,
		},
		{
			name: "openai_chat_completions_v1 request_json invalid json",
			task: &lcpdv1.Task{
				Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
					OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
						RequestJson: []byte(`{`),
						Params:      &lcpdv1.OpenAIChatCompletionsV1Params{Model: "gpt-5.2"},
					},
				},
			},
			wantErr: lcptasks.ErrOpenAIChatCompletionsV1RequestJSONInvalid,
		},
		{
			name: "openai_chat_completions_v1 request_json model missing",
			task: &lcpdv1.Task{
				Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
					OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
						RequestJson: []byte(`{"stream":false}`),
						Params:      &lcpdv1.OpenAIChatCompletionsV1Params{Model: "gpt-5.2"},
					},
				},
			},
			wantErr: lcptasks.ErrOpenAIChatCompletionsV1RequestModelRequired,
		},
		{
			name: "openai_chat_completions_v1 request_json messages missing",
			task: &lcpdv1.Task{
				Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
					OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
						RequestJson: []byte(`{"model":"gpt-5.2"}`),
						Params:      &lcpdv1.OpenAIChatCompletionsV1Params{Model: "gpt-5.2"},
					},
				},
			},
			wantErr: lcptasks.ErrOpenAIChatCompletionsV1RequestMessagesRequired,
		},
		{
			name: "openai_chat_completions_v1 model mismatch",
			task: &lcpdv1.Task{
				Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
					OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
						RequestJson: []byte(`{"model":"gpt-5.2","messages":[{}]}`),
						Params:      &lcpdv1.OpenAIChatCompletionsV1Params{Model: "other"},
					},
				},
			},
			wantErr: lcptasks.ErrOpenAIChatCompletionsV1ModelMismatch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := lcptasks.ValidateTask(tc.task)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateTask error mismatch (-want +got):\n%s", cmp.Diff(tc.wantErr, err))
			}
		})
	}
}

func TestValidateTask_OpenAIChatCompletionsV1_AllowsStreaming(t *testing.T) {
	t.Parallel()

	requestJSON := []byte(
		`{"model":"gpt-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
	)

	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
			OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
				RequestJson: requestJSON,
				Params: &lcpdv1.OpenAIChatCompletionsV1Params{
					Model: "gpt-5.2",
				},
			},
		},
	}

	if err := lcptasks.ValidateTask(task); err != nil {
		t.Fatalf("ValidateTask: %v", err)
	}
}

func TestValidateTask_OpenAIResponsesV1(t *testing.T) {
	t.Parallel()

	requestJSON := []byte(`{"model":"gpt-5.2","input":"hi"}`)

	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_OpenaiResponsesV1{
			OpenaiResponsesV1: &lcpdv1.OpenAIResponsesV1TaskSpec{
				RequestJson: requestJSON,
				Params: &lcpdv1.OpenAIResponsesV1Params{
					Model: "gpt-5.2",
				},
			},
		},
	}

	if err := lcptasks.ValidateTask(task); err != nil {
		t.Fatalf("ValidateTask: %v", err)
	}
}

func TestToWireQuoteRequestTask_OpenAIChatCompletionsV1(t *testing.T) {
	t.Parallel()

	requestJSON := []byte(`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}]}`)
	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
			OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
				RequestJson: requestJSON,
				Params:      &lcpdv1.OpenAIChatCompletionsV1Params{Model: "gpt-5.2"},
			},
		},
	}

	got, err := lcptasks.ToWireQuoteRequestTask(task)
	if err != nil {
		t.Fatalf("ToWireQuoteRequestTask: %v", err)
	}

	if diff := cmp.Diff(lcptasks.TaskKindOpenAIChatCompletionsV1, got.TaskKind); diff != "" {
		t.Fatalf("TaskKind mismatch (-want +got):\n%s", diff)
	}

	decoded, err := lcpwire.DecodeOpenAIChatCompletionsV1Params(got.ParamsBytes)
	if err != nil {
		t.Fatalf("DecodeOpenAIChatCompletionsV1Params: %v", err)
	}

	wantParams := lcpwire.OpenAIChatCompletionsV1Params{Model: "gpt-5.2"}
	if diff := cmp.Diff(wantParams, decoded); diff != "" {
		t.Fatalf("params mismatch (-want +got):\n%s", diff)
	}
}

func TestToWireQuoteRequestTask_OpenAIResponsesV1(t *testing.T) {
	t.Parallel()

	requestJSON := []byte(`{"model":"gpt-5.2","input":"hi"}`)
	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_OpenaiResponsesV1{
			OpenaiResponsesV1: &lcpdv1.OpenAIResponsesV1TaskSpec{
				RequestJson: requestJSON,
				Params:      &lcpdv1.OpenAIResponsesV1Params{Model: "gpt-5.2"},
			},
		},
	}

	got, err := lcptasks.ToWireQuoteRequestTask(task)
	if err != nil {
		t.Fatalf("ToWireQuoteRequestTask: %v", err)
	}

	if diff := cmp.Diff(lcptasks.TaskKindOpenAIResponsesV1, got.TaskKind); diff != "" {
		t.Fatalf("TaskKind mismatch (-want +got):\n%s", diff)
	}

	decoded, err := lcpwire.DecodeOpenAIResponsesV1Params(got.ParamsBytes)
	if err != nil {
		t.Fatalf("DecodeOpenAIResponsesV1Params: %v", err)
	}

	wantParams := lcpwire.OpenAIResponsesV1Params{Model: "gpt-5.2"}
	if diff := cmp.Diff(wantParams, decoded); diff != "" {
		t.Fatalf("params mismatch (-want +got):\n%s", diff)
	}
}

func TestToWireInputStream_OpenAIChatCompletionsV1(t *testing.T) {
	t.Parallel()

	requestJSON := []byte(`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}]}`)
	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
			OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
				RequestJson: requestJSON,
				Params:      &lcpdv1.OpenAIChatCompletionsV1Params{Model: "gpt-5.2"},
			},
		},
	}

	got, err := lcptasks.ToWireInputStream(task)
	if err != nil {
		t.Fatalf("ToWireInputStream: %v", err)
	}

	want := lcptasks.InputStream{
		DecodedBytes:    requestJSON,
		ContentType:     lcptasks.ContentTypeApplicationJSONUTF8,
		ContentEncoding: lcptasks.ContentEncodingIdentity,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("input stream mismatch (-want +got):\n%s", diff)
	}
}

func TestToWireInputStream_OpenAIResponsesV1(t *testing.T) {
	t.Parallel()

	requestJSON := []byte(`{"model":"gpt-5.2","input":"hi"}`)
	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_OpenaiResponsesV1{
			OpenaiResponsesV1: &lcpdv1.OpenAIResponsesV1TaskSpec{
				RequestJson: requestJSON,
				Params:      &lcpdv1.OpenAIResponsesV1Params{Model: "gpt-5.2"},
			},
		},
	}

	got, err := lcptasks.ToWireInputStream(task)
	if err != nil {
		t.Fatalf("ToWireInputStream: %v", err)
	}

	want := lcptasks.InputStream{
		DecodedBytes:    requestJSON,
		ContentType:     lcptasks.ContentTypeApplicationJSONUTF8,
		ContentEncoding: lcptasks.ContentEncodingIdentity,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("input stream mismatch (-want +got):\n%s", diff)
	}
}
