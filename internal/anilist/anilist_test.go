package anilist

import (
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestDedupeTitles(t *testing.T) {
	got := dedupeTitles("Frieren", "", "Frieren", "Sousou no Frieren")
	want := []string{"Frieren", "Sousou no Frieren"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dedupeTitles() = %v, want %v", got, want)
	}
}

func TestParseMedia(t *testing.T) {
	raw := []byte(`{"data":{"Media":{"format":"TV","seasonYear":2023,"title":{"romaji":"Sousou no Frieren","english":"Frieren","native":"x"}}}}`)
	m, err := parseMedia(raw)
	if err != nil {
		t.Fatalf("parseMedia: %v", err)
	}
	if m.Format != "TV" || m.Year != 2023 {
		t.Errorf("format/year = %q/%d, want TV/2023", m.Format, m.Year)
	}
	want := []string{"Sousou no Frieren", "Frieren", "x"}
	if !reflect.DeepEqual(m.Titles, want) {
		t.Errorf("titles = %v, want %v", m.Titles, want)
	}
}

func TestParseMediaYearFallsBackToStartDate(t *testing.T) {
	raw := []byte(`{"data":{"Media":{"format":"MOVIE","startDate":{"year":2020},"title":{"romaji":"A"}}}}`)
	m, err := parseMedia(raw)
	if err != nil {
		t.Fatalf("parseMedia: %v", err)
	}
	if m.Year != 2020 {
		t.Errorf("year = %d, want startDate fallback 2020", m.Year)
	}
}

func TestParseMediaNotFound(t *testing.T) {
	raw := []byte(`{"data":{"Media":null}}`)
	if _, err := parseMedia(raw); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestParseMediaNotFoundCarriesMessage(t *testing.T) {
	raw := []byte(`{"data":{"Media":null},"errors":[{"message":"Not Found."}]}`)
	_, err := parseMedia(raw)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestParseMediaPage(t *testing.T) {
	raw := []byte(`{"data":{"Page":{"media":[` +
		`{"id":1,"format":"TV","seasonYear":2023,"title":{"romaji":"A"}},` +
		`{"id":2,"format":"MOVIE","startDate":{"year":2019},"title":{"romaji":"B","english":"B"}}` +
		`]}}}`)
	out, err := parseMediaPage(raw)
	if err != nil {
		t.Fatalf("parseMediaPage: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[1].Year != 2023 {
		t.Errorf("id 1 year = %d, want 2023", out[1].Year)
	}
	if out[2].Year != 2019 {
		t.Errorf("id 2 year = %d, want startDate fallback 2019", out[2].Year)
	}
	if !reflect.DeepEqual(out[2].Titles, []string{"B"}) {
		t.Errorf("id 2 titles = %v, want deduped [B]", out[2].Titles)
	}
}

func TestParseMediaPageErrorFailsBatch(t *testing.T) {
	raw := []byte(`{"errors":[{"message":"bad request"}]}`)
	if _, err := parseMediaPage(raw); err == nil {
		t.Error("a GraphQL-level error must fail the batch")
	}
}

func TestObserveRateHeadersCapsResetWindow(t *testing.T) {
	client := NewClient(http.DefaultClient, "https://example.invalid/graphql", 30, nil)
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("X-RateLimit-Remaining", "1")
	resp.Header.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10))

	client.observeRateHeaders(resp)

	wait := client.throttle.reserve()
	if wait > maxRetryAfter {
		t.Errorf("low-budget reset wait = %v, want no more than %v", wait, maxRetryAfter)
	}
	if wait < maxRetryAfter-2*time.Second {
		t.Errorf("low-budget reset wait = %v, want close to capped %v", wait, maxRetryAfter)
	}
}
