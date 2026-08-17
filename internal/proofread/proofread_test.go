package proofread

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeServer stands in for an OpenAI-compatible /chat/completions endpoint,
// returning respond(requestBody) for every call.
func fakeServer(t *testing.T, respond func(body string) (status int, content string)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want the configured key", got)
		}
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}

		status, content := respond(string(buf))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		response := chatResponse{Choices: []struct {
			Message chatMessage `json:"message"`
		}{{Message: chatMessage{Role: "assistant", Content: content}}}}
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(response)
		} else {
			_, _ = w.Write([]byte(content))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFixBatchReturnsOnlyChangedIDs(t *testing.T) {
	server := fakeServer(t, func(body string) (int, string) {
		return http.StatusOK, `{"1": "UNDERSTAND, MY SON, that a man lacks accomplishments"}`
	})
	client := NewClient("test-key", server.URL, "")

	fixes, failed, err := client.FixBatch(context.Background(), []Item{
		{ID: "1", Text: "NDERSTAND, MY SON, that as long as a man U lacks accomplishments"},
		{ID: "2", Text: "a passage with nothing wrong with it"},
	})
	if err != nil {
		t.Fatalf("FixBatch: %v", err)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	if len(fixes) != 1 || fixes["1"] == "" {
		t.Errorf("fixes = %v, want only id 1 corrected", fixes)
	}
	if _, ok := fixes["2"]; ok {
		t.Error("id 2 was not asked to change but got a fix anyway")
	}
}

// TestFixBatchIgnoresIDsOutsideTheBatch guards against a model that echoes
// or hallucinates an id it was never given — the same defence tag_llm.py's
// own parse_batch_response applies to tags.
func TestFixBatchIgnoresIDsOutsideTheBatch(t *testing.T) {
	server := fakeServer(t, func(body string) (int, string) {
		return http.StatusOK, `{"1": "fixed", "999": "should never appear"}`
	})
	client := NewClient("test-key", server.URL, "")

	fixes, _, err := client.FixBatch(context.Background(), []Item{{ID: "1", Text: "text"}})
	if err != nil {
		t.Fatalf("FixBatch: %v", err)
	}
	if _, ok := fixes["999"]; ok {
		t.Error("a fix for an id outside the batch was not filtered out")
	}
}

// TestFixBatchRetriesWithoutResponseFormatOn400 covers the fallback
// tag_llm.py's own call_llm applies: some providers reject response_format
// on a model that does not support it.
func TestFixBatchRetriesWithoutResponseFormatOn400(t *testing.T) {
	attempts := 0
	server := fakeServer(t, func(body string) (int, string) {
		attempts++
		if strings.Contains(body, "response_format") {
			return http.StatusBadRequest, `{"error": "response_format not supported"}`
		}
		return http.StatusOK, `{"1": "fixed"}`
	})
	client := NewClient("test-key", server.URL, "")

	fixes, failed, err := client.FixBatch(context.Background(), []Item{{ID: "1", Text: "text"}})
	if err != nil {
		t.Fatalf("FixBatch: %v", err)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want the retry to have succeeded", failed)
	}
	if fixes["1"] != "fixed" {
		t.Errorf("fixes = %v, want id 1 fixed after the retry", fixes)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want exactly one retry", attempts)
	}
}

// TestFixBatchSplitsIntoChunks covers a batch larger than BatchSize: it must
// take more than one request, and every item must still come back.
func TestFixBatchSplitsIntoChunks(t *testing.T) {
	requests := 0
	server := fakeServer(t, func(body string) (int, string) {
		requests++
		payload := decodeItems(t, body)
		fixes := make(map[string]string, len(payload))
		for _, item := range payload {
			fixes[item.ID] = item.Text + " (fixed)"
		}
		encoded, _ := json.Marshal(fixes)
		return http.StatusOK, string(encoded)
	})
	client := NewClient("test-key", server.URL, "")

	items := make([]Item, BatchSize+3)
	for i := range items {
		items[i] = Item{ID: itoa(i), Text: "text"}
	}

	fixes, failed, err := client.FixBatch(context.Background(), items)
	if err != nil {
		t.Fatalf("FixBatch: %v", err)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	if requests != 2 {
		t.Errorf("requests = %d, want 2 batches for %d items", requests, len(items))
	}
	if len(fixes) != len(items) {
		t.Errorf("got %d fixes, want all %d items back", len(fixes), len(items))
	}
}

// TestFixBatchCountsFailedBatchesWithoutFailingTheWhole ensures one bad
// batch does not lose the results of the others.
func TestFixBatchCountsFailedBatchesWithoutFailingTheWhole(t *testing.T) {
	requests := 0
	server := fakeServer(t, func(body string) (int, string) {
		requests++
		if requests == 1 {
			return http.StatusInternalServerError, `{"error": "boom"}`
		}
		payload := decodeItems(t, body)
		fixes := make(map[string]string, len(payload))
		for _, item := range payload {
			fixes[item.ID] = "fixed"
		}
		encoded, _ := json.Marshal(fixes)
		return http.StatusOK, string(encoded)
	})
	client := NewClient("test-key", server.URL, "")

	items := make([]Item, BatchSize+1)
	for i := range items {
		items[i] = Item{ID: itoa(i), Text: "text"}
	}

	fixes, failed, err := client.FixBatch(context.Background(), items)
	if err != nil {
		t.Fatalf("FixBatch: %v", err)
	}
	if failed != BatchSize {
		t.Errorf("failed = %d, want the first batch's %d items counted", failed, BatchSize)
	}
	if len(fixes) == 0 {
		t.Error("the second batch's fix was lost along with the first batch's failure")
	}
}

func TestConfiguredReportsWhetherAKeyIsSet(t *testing.T) {
	if (NewClient("", "", "")).Configured() {
		t.Error("Configured = true with no key")
	}
	if !(NewClient("a-key", "", "")).Configured() {
		t.Error("Configured = false with a key set")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// decodeItems recovers the []itemJSON a fixChunk request embedded inside its
// user message's own text content — the array itself is JSON, but arrives as
// the tail of a plain-text instruction, not as a request field of its own.
func decodeItems(t *testing.T, body string) []itemJSON {
	t.Helper()
	var request chatRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(request.Messages) != 2 {
		t.Fatalf("got %d messages, want a system and a user message", len(request.Messages))
	}
	content := request.Messages[1].Content
	start := strings.Index(content, "[")
	if start == -1 {
		t.Fatalf("user message has no JSON array: %s", content)
	}
	var payload []itemJSON
	if err := json.Unmarshal([]byte(content[start:]), &payload); err != nil {
		t.Fatalf("decode request payload: %v", err)
	}
	return payload
}
