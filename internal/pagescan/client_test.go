package pagescan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeRequest reads and re-marshals the request body as a generic map, so
// tests can assert on individual JSON keys without depending on the
// unexported request struct's field order or Go-side naming.
func decodeRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return decoded
}

func TestReadPageRequestShape(t *testing.T) {
	var captured map[string]any
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = decodeRequest(t, r)
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"pages\":[]}"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	client := NewClient("test-key", server.URL, "some/model", "low")
	content, err := client.ReadPage(context.Background(), []byte{0xFF, 0xD8}, "image/jpeg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != `{"pages":[]}` {
		t.Errorf("content: got %q", content)
	}

	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization: got %q, want %q", gotAuth, "Bearer test-key")
	}
	if captured["model"] != "some/model" {
		t.Errorf("model: got %v, want some/model", captured["model"])
	}
	if captured["temperature"] != float64(0) {
		t.Errorf("temperature: got %v, want 0", captured["temperature"])
	}
	if captured["max_tokens"] != float64(MaxTokens) {
		t.Errorf("max_tokens: got %v, want %d", captured["max_tokens"], MaxTokens)
	}
	reasoning, ok := captured["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning: got %v, want an object", captured["reasoning"])
	}
	if reasoning["effort"] != "low" {
		t.Errorf("reasoning.effort: got %v, want low", reasoning["effort"])
	}
	responseFormat, ok := captured["response_format"].(map[string]any)
	if !ok || responseFormat["type"] != "json_object" {
		t.Errorf("response_format: got %v, want {type: json_object}", captured["response_format"])
	}

	messages, ok := captured["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages: got %v", captured["messages"])
	}
	message := messages[0].(map[string]any)
	contentParts := message["content"].([]any)
	if len(contentParts) != 2 {
		t.Fatalf("content parts: got %d, want 2", len(contentParts))
	}
	imagePart := contentParts[1].(map[string]any)
	imageURL := imagePart["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(imageURL, "data:image/jpeg;base64,") {
		t.Errorf("image_url: got %q, want a data:image/jpeg;base64,... prefix", imageURL)
	}
}

func TestReadPageReasoningNone(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = decodeRequest(t, r)
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"pages\":[]}"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	client := NewClient("key", server.URL, "model", "none")
	if _, err := client.ReadPage(context.Background(), []byte{1}, "image/png"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	reasoning, ok := captured["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning: got %v, want an object", captured["reasoning"])
	}
	enabled, present := reasoning["enabled"]
	if !present || enabled != false {
		t.Errorf("reasoning: got %v, want {enabled: false}", reasoning)
	}
	if _, hasEffort := reasoning["effort"]; hasEffort {
		t.Errorf("reasoning: got an effort key too: %v", reasoning)
	}
}

func TestReadPageRetriesWithout400ResponseFormat(t *testing.T) {
	attempt := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		body := decodeRequest(t, r)
		if attempt == 1 {
			if _, has := body["response_format"]; !has {
				t.Errorf("first attempt: expected response_format to be present")
			}
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"response_format not supported"}`))
			return
		}
		if _, has := body["response_format"]; has {
			t.Errorf("retry: expected response_format to be dropped")
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"pages\":[]}"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	client := NewClient("key", server.URL, "model", "low")
	content, err := client.ReadPage(context.Background(), []byte{1}, "image/png")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != `{"pages":[]}` {
		t.Errorf("content: got %q", content)
	}
	if attempt != 2 {
		t.Fatalf("got %d attempts, want 2 (one retry after the 400)", attempt)
	}
}

func TestReadPageFinishReasonLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"pages\":"},"finish_reason":"length"}]}`))
	}))
	defer server.Close()

	client := NewClient("key", server.URL, "model", "low")
	_, err := client.ReadPage(context.Background(), []byte{1}, "image/png")
	if err == nil {
		t.Fatal("expected an error for finish_reason=length")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should mention tokens (the runaway-thinking failure), got: %v", err)
	}
}

func TestReadPageFencedContentAccepted(t *testing.T) {
	fenced := "```json\n{\"pages\":[]}\n```"
	responseBody, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]any{"content": fenced},
			"finish_reason": "stop",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(responseBody)
	}))
	defer server.Close()

	client := NewClient("key", server.URL, "model", "low")
	content, err := client.ReadPage(context.Background(), []byte{1}, "image/png")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(content, "```") {
		t.Errorf("expected the raw fenced content back, got %q", content)
	}
	if _, err := ParseResult(content); err != nil {
		t.Errorf("returned content should itself parse: %v", err)
	}
}

func TestReadPageNon200IncludesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("upstream exploded"))
	}))
	defer server.Close()

	client := NewClient("key", server.URL, "model", "low")
	_, err := client.ReadPage(context.Background(), []byte{1}, "image/png")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should include the status code, got: %v", err)
	}
}

func TestNewClientDefaults(t *testing.T) {
	client := NewClient("key", "", "", "")
	if client.baseURL != DefaultBaseURL {
		t.Errorf("baseURL: got %q, want %q", client.baseURL, DefaultBaseURL)
	}
	if client.model != DefaultModel {
		t.Errorf("model: got %q, want %q", client.model, DefaultModel)
	}
	if client.effort != DefaultEffort {
		t.Errorf("effort: got %q, want %q", client.effort, DefaultEffort)
	}
	if client.Model() != DefaultModel {
		t.Errorf("Model(): got %q, want %q", client.Model(), DefaultModel)
	}
}

func TestClientConfigured(t *testing.T) {
	if (&Client{}).Configured() {
		t.Error("empty Client should not be Configured")
	}
	if !NewClient("key", "", "", "").Configured() {
		t.Error("Client with an apiKey should be Configured")
	}
}
