package testapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// FakeCC stands in for cause-channel's POST /v1/causes (another module) with
// the same body contract; it records every cause and can be made to fail.
type FakeCC struct {
	URL string

	mu     sync.Mutex
	causes []map[string]any
	fail   bool
}

func StartCC(t *testing.T) *FakeCC {
	t.Helper()
	f := &FakeCC{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/causes" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		f.causes = append(f.causes, body)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "01CAUSE" + strconv.Itoa(len(f.causes))})
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// SetFail makes every following POST /v1/causes answer 503.
func (f *FakeCC) SetFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

// Causes returns a copy of the causes issued so far.
func (f *FakeCC) Causes() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.causes...)
}
