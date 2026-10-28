package tracker

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"arknet.local/tracker/internal/grant"
)

func makeGrant(t *testing.T, privateKey ed25519.PrivateKey, hash [32]byte, expiry time.Time) string {
	return makeGrantForSubject(t, privateKey, hash, expiry, [16]byte{})
}

func makeGrantForSubject(t *testing.T, privateKey ed25519.PrivateKey, hash [32]byte, expiry time.Time, subject [16]byte) string {
	return makeGrantForSubjectAndActions(t, privateKey, hash, expiry, subject, grant.ActionDownload|grant.ActionSeed)
}

func makeGrantForSubjectAndActions(t *testing.T, privateKey ed25519.PrivateKey, hash [32]byte, expiry time.Time, subject [16]byte, actions byte) string {
	t.Helper()
	payload := make([]byte, grant.PayloadSize)
	payload[0] = grant.Version
	binary.BigEndian.PutUint64(payload[1:9], uint64(expiry.Unix()))
	copy(payload[9:25], subject[:])
	copy(payload[25:57], hash[:])
	payload[57] = actions
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
}

func subjectFor(index uint64) [16]byte {
	var subject [16]byte
	binary.BigEndian.PutUint64(subject[8:], index)
	return subject
}

func announceQuery(hash [32]byte, peerID string, port string) string {
	values := url.Values{
		"info_hash":  {string(hash[:20])},
		"peer_id":    {peerID},
		"port":       {port},
		"uploaded":   {"0"},
		"downloaded": {"0"},
		"left":       {"1"},
		"compact":    {"1"},
	}
	return values.Encode()
}

func TestHandlerRejectsOversizedRequestTarget(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(publicKey, time.Minute, time.Minute, time.Now)
	request := httptest.NewRequest(http.MethodGet, "/health?padding="+string(bytes.Repeat([]byte{'a'}, 9000)), nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestURITooLong {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestURITooLong)
	}
}

func TestHealth(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(publicKey, 30*time.Minute, 45*time.Minute, time.Now)
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if response.Body.String() != "ok\n" {
		t.Fatalf("body = %q", response.Body.String())
	}
}

func TestAnnounceAcceptsLibtorrentV2OptionalFields(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 1
	token := makeGrant(t, privateKey, hash, now.Add(time.Hour))
	values, err := url.ParseQuery(announceQuery(hash, "-ARKNET-PEER-0000001", "6881"))
	if err != nil {
		t.Fatal(err)
	}
	values.Set("corrupt", "0")
	values.Set("key", "deadbeef")
	values.Set("event", "started")
	values.Set("numwant", "200")
	values.Set("no_peer_id", "1")
	values.Set("supportcrypto", "1")
	values.Set("redundant", "0")
	handler := NewHandler(publicKey, 30*time.Minute, 45*time.Minute, func() time.Time { return now })
	request := httptest.NewRequest(http.MethodGet, "/announce/"+token+"?"+values.Encode(), nil)
	request.RemoteAddr = "192.0.2.10:54321"
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestAnnounceRejectsUnknownQueryParameter(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 1
	token := makeGrant(t, privateKey, hash, now.Add(time.Hour))
	handler := NewHandler(publicKey, 30*time.Minute, 45*time.Minute, func() time.Time { return now })
	request := httptest.NewRequest(http.MethodGet, "/announce/"+token+"?"+announceQuery(hash, "-ARKNET-PEER-0000001", "6881")+"&ip=203.0.113.5", nil)
	request.RemoteAddr = "192.0.2.10:54321"
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestAnnounceRegistersPeerAndReturnsCompactResponse(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	token := makeGrant(t, privateKey, hash, now.Add(time.Hour))
	handler := NewHandler(publicKey, 30*time.Minute, 45*time.Minute, func() time.Time { return now })
	request := httptest.NewRequest(http.MethodGet, "/announce/"+token+"?"+announceQuery(hash, "-ARKNET-PEER-0000001", "6881"), nil)
	request.RemoteAddr = "192.0.2.10:54321"
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q", got)
	}
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	want := "d8:completei0e10:incompletei1e8:intervali1800e5:peers0:6:peers60:e"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

// seedQuery announces a complete copy (left=0), which requires a seed grant.
func seedQuery(hash [32]byte, peerID string, port string) string {
	values, _ := url.ParseQuery(announceQuery(hash, peerID, port))
	values.Set("left", "0")
	return values.Encode()
}
