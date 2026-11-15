package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// codexSearchBody is the SearchRequest Codex's web.run sends, as its own
// test (codex-api/src/endpoint/search.rs) has it on the wire, on model.
func codexSearchBody(model string) string {
	return `{"id":"search-session","model":"` + model + `","input":[{"type":"message","id":"msg_search","role":"user","content":[{"type":"input_text","text":"find this"},{"type":"input_image","image_url":"https://example.com/image.png"}]}],"commands":{"search_query":[{"q":"OpenAI news","recency":7,"domains":["openai.com"]}],"open":[{"ref_id":"https://openai.com","lineno":12}]},"settings":{"user_location":{"type":"approximate","country":"US","city":"San Francisco"},"search_context_size":"low","filters":{"allowed_domains":["openai.com"],"blocked_domains":["example.com"]},"image_settings":{"max_results":4,"caption":true},"allowed_callers":["direct"],"external_web_access":true},"max_output_tokens":2500}`
}

// codexSearchPost sends a search to path as Codex sends it to magpie's
// provider: its bearer token and actor header from the provider table,
// its turn metadata and originator of its own.
func codexSearchPost(h http.Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer magpie")
	r.Header.Set("x-openai-actor-authorization", "magpie")
	r.Header.Set("x-codex-turn-metadata", `{"turn_id":"t1"}`)
	r.Header.Set("originator", "codex_exec")
	r.Header.Set("User-Agent", "codex_exec/0.162.1 (Linux 6.8; x86_64)")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type searchReply struct {
	EncryptedOutput *string          `json:"encrypted_output"`
	Output          string           `json:"output"`
	Results         []map[string]any `json:"results"`
}

func readSearchReply(t *testing.T, w *httptest.ResponseRecorder) searchReply {
	t.Helper()
	var v searchReply
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	return v
}

// Codex's web.run on a ChatGPT account's model in magpie posted to
// /v1/alpha/search and got 404, which ended the turn (anton on Discord).
// It goes to the ChatGPT backend's search as Codex sends it there itself:
// on the account's sign-in, the model as the backend knows it, the rest
// of the request as it came; an account out of its allowance passes it
// to the next, and the backend's answer goes back as it is.
func TestCodexWebRunSearchesOnChatGPTAccount(t *testing.T) {
	codexSignedIn(t, "spare@example.com")
	var mu sync.Mutex
	var paths, tried []string
	var heads []http.Header
	var bodies []map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var q map[string]any
		json.Unmarshal(b, &q)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		tried = append(tried, r.Header.Get("chatgpt-account-id"))
		heads = append(heads, r.Header.Clone())
		bodies = append(bodies, q)
		mu.Unlock()
		if r.Header.Get("chatgpt-account-id") == "acct-1" {
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"encrypted_output":"ciphertext","output":"OpenAI news from acct-2","results":[{"type":"text_result","ref_id":"turn0search0","url":"https://example.com/result"}]}`)
	}))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })

	h := New().Handler()
	for _, path := range []string{"/v1/alpha/search", CodexPath + "/alpha/search"} {
		mu.Lock()
		paths, tried, heads, bodies = nil, nil, nil, nil
		mu.Unlock()
		got := readSearchReply(t, codexSearchPost(h, path, codexSearchBody("codex/gpt-5.5")))
		if got.Output != "OpenAI news from acct-2" || got.EncryptedOutput == nil || *got.EncryptedOutput != "ciphertext" || len(got.Results) != 1 {
			t.Fatalf("%s: answered %+v", path, got)
		}
		if want := map[string]any{"type": "text_result", "ref_id": "turn0search0", "url": "https://example.com/result"}; !reflect.DeepEqual(got.Results[0], want) {
			t.Fatalf("%s: upstream source changed: %v", path, got.Results)
		}
		if strings.Join(tried, ",") != "acct-1,acct-2" {
			t.Fatalf("%s: accounts asked %v", path, tried)
		}
		for i, hd := range heads {
			if paths[i] != "/backend-api/codex/alpha/search" {
				t.Fatalf("%s: went to %s", path, paths[i])
			}
			if a := hd.Get("Authorization"); a == "" || a == "Bearer magpie" || hd.Get("x-openai-actor-authorization") != "" {
				t.Fatalf("%s: magpie's own token or actor header went on: %v", path, hd)
			}
			if hd.Get("x-codex-turn-metadata") != `{"turn_id":"t1"}` || hd.Get("originator") != "codex_cli_rs" || hd.Get("Accept") == "text/event-stream" {
				t.Fatalf("%s: headers %v", path, hd)
			}
			q := bodies[i]
			cmds, _ := q["commands"].(map[string]any)
			if q["model"] != "gpt-5.5" || q["id"] != "search-session" || cmds["open"] == nil || q["settings"] == nil || q["max_output_tokens"] != float64(2500) {
				t.Fatalf("%s: body %v", path, q)
			}
		}
	}
}

// Every ChatGPT account failing ends no turn: magpie searches in its
// place, and the model is told why.
func TestCodexWebRunChatGPTFailsSearchedByMagpie(t *testing.T) {
	codexSignedIn(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"down"}`, 500)
	}))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	apis := newSearchAPIs(t)
	if err := provider.SetSearchAPI(apis.api("tavily", "tvly-k")); err != nil {
		t.Fatal(err)
	}
	st := settings.Load()
	st.Searcher = "off"
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}
	got := readSearchReply(t, codexSearchPost(New().Handler(), "/v1/alpha/search", codexSearchBody("codex/gpt-5.5")))
	if !strings.Contains(got.Output, "ChatGPT's web search failed") || !strings.Contains(got.Output, "500") ||
		!strings.Contains(got.Output, "go1.27.1 is the latest release.") {
		t.Fatalf("answered %q", got.Output)
	}
	if want := []map[string]any{{"type": "text_result", "url": "https://go.dev/dl/", "title": "Go downloads"}}; !reflect.DeepEqual(got.Results, want) {
		t.Fatalf("fallback sources: got %v, want %v", got.Results, want)
	}
}

// web.run on a model no ChatGPT account serves: its search_query is
// searched as magpie searches for a model that can't, its domains kept;
// with nothing to search with, or nothing to search, the model is told
// what to do, never a failure that ends the turn.
func TestCodexWebRunOnOtherModelSearchedByMagpie(t *testing.T) {
	fresh(t)
	// nothing of it goes to the ChatGPT backend, which a relay would reach
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("reached the ChatGPT backend: %s %s", r.Method, r.URL.Path)
		http.Error(w, "no", 500)
	}))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	h := New().Handler()
	got := readSearchReply(t, codexSearchPost(h, "/v1/alpha/search", codexSearchBody("deepseek/deepseek-v4")))
	if !strings.Contains(got.Output, "Settings → Models → Web search") || got.EncryptedOutput != nil || len(got.Results) != 0 {
		t.Fatalf("no searcher: %+v", got)
	}
	apis := newSearchAPIs(t)
	if err := provider.SetSearchAPI(apis.api("tavily", "tvly-k")); err != nil {
		t.Fatal(err)
	}
	got = readSearchReply(t, codexSearchPost(h, "/v1/alpha/search", codexSearchBody("deepseek/deepseek-v4")))
	if !strings.Contains(got.Output, "Go downloads") || !strings.Contains(got.Output, "go1.27.1 is the latest release.") {
		t.Fatalf("searched: %+v", got)
	}
	want := []map[string]any{{"type": "text_result", "url": "https://go.dev/dl/", "title": "Go downloads"}}
	if !reflect.DeepEqual(got.Results, want) {
		t.Fatalf("sources: got %v, want %v", got.Results, want)
	}
	if strings.Join(apis.asked, "|") != "tavily: OpenAI news site:openai.com" {
		t.Fatalf("asked %q", apis.asked)
	}
	// a magpie model through Codex's ChatGPT path is searched the same
	got = readSearchReply(t, codexSearchPost(h, CodexPath+"/alpha/search", codexSearchBody("deepseek/deepseek-v4")))
	if !strings.Contains(got.Output, "go1.27.1") || !reflect.DeepEqual(got.Results, want) {
		t.Fatalf("on %s: %+v", CodexPath, got)
	}
	got = readSearchReply(t, codexSearchPost(h, "/v1/alpha/search", `{"id":"s","model":"deepseek/deepseek-v4","commands":{"open":[{"ref_id":"https://openai.com"}]}}`))
	if !strings.Contains(got.Output, "search_query") || len(got.Results) != 0 {
		t.Fatalf("open alone: %+v", got)
	}
	if w := codexSearchPost(h, "/v1/alpha/search", `nope`); w.Code != 400 {
		t.Fatalf("not JSON: %d %s", w.Code, w.Body.String())
	}
}

// A failed or empty query loses none of its siblings' sources. The text
// still tells the model what each query found, including each failure.
func TestCodexWebRunKeepsSourcesAcrossQueries(t *testing.T) {
	fresh(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch q.Query {
		case "Go downloads":
			io.WriteString(w, `{"results":[{"title":"Go downloads","url":"https://go.dev/dl/","content":"go1.27.1 is the latest release."}]}`)
		case "Go docs":
			io.WriteString(w, `{"results":[{"title":"Go documentation","url":"https://go.dev/doc/","content":"The Go programming language documentation."}]}`)
		case "empty":
			io.WriteString(w, `{"results":[]}`)
		case "failed":
			http.Error(w, `{"detail":{"error":"down"}}`, http.StatusInternalServerError)
		default:
			t.Errorf("unexpected query %q", q.Query)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(up.Close)
	if err := provider.SetSearchAPI(provider.SearchAPI{Vendor: "tavily", Key: "test-key", URL: up.URL}); err != nil {
		t.Fatal(err)
	}
	st := settings.Load()
	st.Searcher = "off"
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}
	h := New().Handler()
	for _, path := range []string{"/alpha/search", "/v1/alpha/search", CodexPath + "/alpha/search"} {
		got := readSearchReply(t, codexSearchPost(h, path, `{"model":"deepseek/deepseek-v4","commands":{"search_query":[{"q":"Go downloads"},{"q":"empty"},{"q":"failed"},{"q":"Go docs"}]}}`))
		want := []map[string]any{
			{"type": "text_result", "url": "https://go.dev/dl/", "title": "Go downloads"},
			{"type": "text_result", "url": "https://go.dev/doc/", "title": "Go documentation"},
		}
		if !reflect.DeepEqual(got.Results, want) {
			t.Errorf("%s: sources got %v, want %v", path, got.Results, want)
		}
		for _, text := range []string{"Search: Go downloads\n", "go1.27.1 is the latest release.", "couldn't search the web for empty", "found nothing", "couldn't search the web for failed", "down", "Search: Go docs\n", "The Go programming language documentation."} {
			if !strings.Contains(got.Output, text) {
				t.Errorf("%s: output lacks %q: %s", path, text, got.Output)
			}
		}
		got = readSearchReply(t, codexSearchPost(h, path, `{"model":"deepseek/deepseek-v4","commands":{"search_query":[{"q":"empty"}]}}`))
		if len(got.Results) != 0 || !strings.Contains(got.Output, "found nothing") {
			t.Errorf("%s: empty search invented sources or lost its explanation: %+v", path, got)
		}
	}
}
