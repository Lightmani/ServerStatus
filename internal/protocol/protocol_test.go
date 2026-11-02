package protocol

import (
	"bytes"
	"net/http"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	body := []byte(`{"time":1}`)
	req, _ := http.NewRequest(http.MethodPost, "http://x"+ReportPath, bytes.NewReader(body))
	SignRequest(req, ReportPath, "node1", "s3cret", body, time.Unix(1700000000, 0))

	s, err := ParseHeaders(req.Header)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Verify("s3cret", http.MethodPost, ReportPath, body) {
		t.Fatal("valid signature rejected")
	}
	if s.Verify("wrong", http.MethodPost, ReportPath, body) {
		t.Fatal("wrong secret accepted")
	}
	if s.Verify("s3cret", http.MethodPost, ReportPath, []byte(`{"time":2}`)) {
		t.Fatal("tampered body accepted")
	}
	if s.Verify("s3cret", http.MethodPost, "/api/other", body) {
		t.Fatal("different path accepted")
	}
	s2 := s
	s2.Timestamp++
	if s2.Verify("s3cret", http.MethodPost, ReportPath, body) {
		t.Fatal("tampered timestamp accepted")
	}
}

func TestParseHeadersRejectsGarbage(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderNode, "n")
	h.Set(HeaderTimestamp, "abc")
	h.Set(HeaderNonce, "00112233445566778899aabbccddeeff")
	h.Set(HeaderSignature, "zz")
	if _, err := ParseHeaders(h); err == nil {
		t.Fatal("expected error")
	}
}

func TestResponseSignature(t *testing.T) {
	body := []byte(`{"ping_targets":[]}`)
	sig := SignResponse("k", "n", 100, "abcd", body)
	if !VerifyResponse("k", "n", 100, "abcd", body, sig) {
		t.Fatal("valid response rejected")
	}
	if VerifyResponse("k", "n", 100, "other", body, sig) || VerifyResponse("k", "n", 100, "abcd", []byte("{}"), sig) {
		t.Fatal("mismatched response accepted")
	}
	// a request signature must never validate as a response signature
	if VerifyResponse("k", "n", 100, "abcd", body, Sign("k", "POST", ReportPath, "n", 100, "abcd", body)) {
		t.Fatal("request signature accepted as response")
	}
}
