package tesseract_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/station/internal/atlas/atlastest"
	"github.com/hollis-labs/station/internal/atlas/contract"
	"github.com/hollis-labs/station/internal/atlas/tesseract"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func questions(n int) []tesseract.Revision {
	out := make([]tesseract.Revision, n)
	for i := range out {
		out[i] = atlastest.Record("question", fmt.Sprintf("ATLAS-Q-%03d", i+1))
	}
	return out
}

func TestRecallAllFollowsCursorToTheEnd(t *testing.T) {
	fake := atlastest.New(t, questions(5)...)
	c := tesseract.New(fake.URL(), "")

	got, complete, err := c.RecallAll(ctx(t), tesseract.RecallRequest{Namespaces: []string{contract.Root + "/*"}, Limit: 2}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || len(got) != 5 {
		t.Fatalf("complete=%v len=%d, want complete with all 5 records", complete, len(got))
	}
	if n := fake.Calls("recall"); n != 3 {
		t.Errorf("recall calls = %d, want 3 pages of 2, 2 and 1", n)
	}
}

func TestRecallAllReportsWhenItStopsEarly(t *testing.T) {
	fake := atlastest.New(t, questions(9)...)
	c := tesseract.New(fake.URL(), "")

	got, complete, err := c.RecallAll(ctx(t), tesseract.RecallRequest{Namespaces: []string{contract.Root + "/*"}, Limit: 2}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Error("complete=true after stopping at the cap; the caller would present a partial set as the whole")
	}
	if len(got) < 3 || len(got) >= 9 {
		t.Errorf("len=%d, want it to stop soon after the cap of 3", len(got))
	}
}

func TestGetCurrentReturnsTheRecord(t *testing.T) {
	fake := atlastest.New(t, questions(2)...)
	c := tesseract.New(fake.URL(), "")

	rev, err := c.GetCurrent(ctx(t), contract.Root+"/questions", "ATLAS-Q-002")
	if err != nil {
		t.Fatal(err)
	}
	if rev.MemoryKey != "ATLAS-Q-002" || rev.Payload.Summary == "" || len(rev.Payload.Data) == 0 {
		t.Errorf("revision = %+v", rev)
	}
}

func TestGetCurrentMissingKeyIsNotFound(t *testing.T) {
	fake := atlastest.New(t, questions(1)...)
	c := tesseract.New(fake.URL(), "")

	_, err := c.GetCurrent(ctx(t), contract.Root+"/questions", "ATLAS-Q-099")
	if !errors.Is(err, tesseract.ErrNotFound) {
		t.Fatalf("err = %v, want it to match ErrNotFound", err)
	}
	if errors.Is(err, tesseract.ErrUnavailable) {
		t.Error("a missing key must not read as Tesseract being down")
	}
	var api *tesseract.APIError
	if !errors.As(err, &api) || api.Code != "not_found" || api.Status != http.StatusNotFound {
		t.Errorf("err = %#v, want the APIError with Tesseract's own code", err)
	}
}

func TestRefusalCarriesTesseractsOwnMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"validation_error","message":"unknown field \"domains\" in request body"}`))
	}))
	defer srv.Close()

	_, err := tesseract.New(srv.URL, "").Recall(ctx(t), tesseract.RecallRequest{Namespaces: []string{contract.Root}})
	if err == nil || !strings.Contains(err.Error(), `unknown field "domains"`) || !strings.Contains(err.Error(), "validation_error") {
		t.Fatalf("err = %v, want Tesseract's code and message through", err)
	}
	if errors.Is(err, tesseract.ErrNotFound) || errors.Is(err, tesseract.ErrUnavailable) {
		t.Errorf("a 400 is neither not-found nor unavailable: %v", err)
	}
}

func TestUnreachableTesseractIsUnavailable(t *testing.T) {
	fake := atlastest.New(t, questions(1)...)
	c := tesseract.New(fake.URL(), "")
	fake.Close()

	_, err := c.GetCurrent(ctx(t), contract.Root+"/questions", "ATLAS-Q-001")
	if !errors.Is(err, tesseract.ErrUnavailable) {
		t.Fatalf("err = %v, want it to match ErrUnavailable", err)
	}
}

func TestBearerTokenIsSentOnlyWhenSet(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if err := tesseract.New(srv.URL, "").Health(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if err := tesseract.New(srv.URL, "secret").Health(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "" || seen[1] != "Bearer secret" {
		t.Errorf("Authorization headers = %q, want none and then a bearer", seen)
	}
}

func TestListNamespaces(t *testing.T) {
	fake := atlastest.New(t,
		atlastest.Record("question", "ATLAS-Q-001"),
		atlastest.Record("asset", "ATLAS-ASSET-001"),
		atlastest.Doc(contract.Root+"/meta", "ATLAS-META-CHARTER"),
		atlastest.Doc("project/tether/knowledge", "unrelated"),
	)
	got, err := tesseract.New(fake.URL(), "").ListNamespaces(ctx(t), contract.Root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{contract.Root + "/questions": true, contract.Root + "/assets": true, contract.Root + "/meta": true}
	if len(got) != len(want) {
		t.Fatalf("namespaces = %v", got)
	}
	for _, ns := range got {
		if !want[ns] {
			t.Errorf("unexpected namespace %q", ns)
		}
	}
}
