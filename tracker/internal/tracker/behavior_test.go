package tracker

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"arknet.local/tracker/internal/grant"
)

func announce(t *testing.T, handler http.Handler, token, query, remote string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/announce/"+token+"?"+query, nil)
	request.RemoteAddr = remote
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestAnnounceRejectsInvalidGrantAndMismatchedTrackerKey(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPrivateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash, otherHash [32]byte
	hash[0], otherHash[0] = 1, 2
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now })
	tests := map[string]struct {
		token  string
		query  string
		status int
	}{
		"malformed":     {token: "malformed", query: announceQuery(hash, "-ARKNET-PEER-0000001", "6881"), status: http.StatusForbidden},
		"expired":       {token: makeGrant(t, privateKey, hash, now), query: announceQuery(hash, "-ARKNET-PEER-0000001", "6881"), status: http.StatusForbidden},
		"wrong signer":  {token: makeGrant(t, otherPrivateKey, hash, now.Add(time.Hour)), query: announceQuery(hash, "-ARKNET-PEER-0000001", "6881"), status: http.StatusForbidden},
		"hash mismatch": {token: makeGrant(t, privateKey, hash, now.Add(time.Hour)), query: announceQuery(otherHash, "-ARKNET-PEER-0000001", "6881"), status: http.StatusBadRequest},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			response := announce(t, handler, testCase.token, testCase.query, "192.0.2.1:50000")
			if response.Code != testCase.status {
				t.Fatalf("status = %d, want %d", response.Code, testCase.status)
			}
		})
	}
}

func TestAnnounceRejectsMalformedRequiredFields(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	token := makeGrant(t, privateKey, hash, now.Add(time.Hour))
	base, _ := url.ParseQuery(announceQuery(hash, "-ARKNET-PEER-0000001", "6881"))
	tests := map[string]func(url.Values){
		"missing hash":      func(v url.Values) { v.Del("info_hash") },
		"short peer id":     func(v url.Values) { v.Set("peer_id", "short") },
		"zero port":         func(v url.Values) { v.Set("port", "0") },
		"large port":        func(v url.Values) { v.Set("port", "65536") },
		"signed uploaded":   func(v url.Values) { v.Set("uploaded", "+1") },
		"noncompact":        func(v url.Values) { v.Set("compact", "0") },
		"invalid event":     func(v url.Values) { v.Set("event", "paused") },
		"duplicate compact": func(v url.Values) { v.Add("compact", "1") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			values, _ := url.ParseQuery(base.Encode())
			mutate(values)
			response := announce(t, NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now }), token, values.Encode(), "192.0.2.1:50000")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d", response.Code)
			}
		})
	}
}

func TestOnlyHealthAndAnnounceRoutesAreExposed(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(publicKey, time.Minute, time.Minute, time.Now)
	for _, path := range []string{"/", "/scrape", "/dht", "/pex", "/metrics"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("path %s status = %d", path, response.Code)
		}
	}
}

func TestAnnounceReturnsOtherIPv4PeerFromRemoteAddressOnly(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 7
	firstToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(1))
	secondToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(2))
	handler := NewHandler(publicKey, time.Minute, 2*time.Minute, func() time.Time { return now })
	firstQuery := seedQuery(hash, "-ARKNET-PEER-0000001", "6881")
	request := httptest.NewRequest(http.MethodGet, "/announce/"+firstToken+"?"+firstQuery, nil)
	request.RemoteAddr = "192.0.2.10:50000"
	request.Header.Set("X-Forwarded-For", "203.0.113.99")
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, request)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first status = %d", firstResponse.Code)
	}

	second := announce(t, handler, secondToken, announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "192.0.2.11:50001")
	want := append([]byte("d8:completei1e10:incompletei1e8:intervali60e5:peers6:"), []byte{192, 0, 2, 10, 0x1a, 0xe1}...)
	want = append(want, []byte("6:peers60:e")...)
	if !bytes.Equal(second.Body.Bytes(), want) {
		t.Fatalf("body = %q, want %q", second.Body.Bytes(), want)
	}
}

func TestAnnounceReturnsIPv6PeersSeparately(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 8
	firstToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(1))
	secondToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(2))
	handler := NewHandler(publicKey, time.Minute, 2*time.Minute, func() time.Time { return now })
	announce(t, handler, firstToken, seedQuery(hash, "-ARKNET-PEER-0000001", "6881"), "[2001:db8::1]:50000")
	second := announce(t, handler, secondToken, announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "[2001:db8::2]:50001")
	ipv6 := []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0x1a, 0xe1}
	want := append([]byte("d8:completei1e10:incompletei1e8:intervali60e5:peers0:6:peers618:"), ipv6...)
	want = append(want, 'e')
	if !bytes.Equal(second.Body.Bytes(), want) {
		t.Fatalf("body = %q, want %q", second.Body.Bytes(), want)
	}
}

func TestStoppedAndStalePeersAreRemoved(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	clock := now
	var hash [32]byte
	hash[0] = 9
	firstToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(1))
	secondToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(2))
	thirdToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(3))
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return clock })
	first := announceQuery(hash, "-ARKNET-PEER-0000001", "6881")
	announce(t, handler, firstToken, first, "192.0.2.10:50000")
	stopped, _ := url.ParseQuery(first)
	stopped.Set("event", "stopped")
	announce(t, handler, firstToken, stopped.Encode(), "192.0.2.10:50000")
	clock = now.Add(time.Minute)
	announce(t, handler, thirdToken, announceQuery(hash, "-ARKNET-PEER-0000003", "6883"), "192.0.2.12:50002")
	third := announce(t, handler, secondToken, announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "192.0.2.11:50001")
	if !bytes.Contains(third.Body.Bytes(), []byte("10:incompletei2e")) {
		t.Fatalf("body = %q, want exactly two active incomplete peers", third.Body.Bytes())
	}
}

func TestPeerIsRemovedWhenGrantExpiresBeforePeerTTL(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	clock := now
	var hash [32]byte
	hash[0] = 17
	firstToken := makeGrantForSubject(t, privateKey, hash, now.Add(10*time.Second), subjectFor(1))
	secondToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(2))
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return clock })
	announce(t, handler, firstToken, announceQuery(hash, "-ARKNET-PEER-0000001", "6881"), "192.0.2.10:50000")

	clock = now.Add(10 * time.Second)
	response := announce(t, handler, secondToken, announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "192.0.2.11:50001")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("10:incompletei1e")) || !bytes.Contains(response.Body.Bytes(), []byte("5:peers0:")) {
		t.Fatalf("body = %q, want only announcing peer after first peer grant expires", response.Body.Bytes())
	}
}

func TestFullV2HashesWithSameTrackerKeyUseSeparateSwarms(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var firstHash, secondHash [32]byte
	for index := 0; index < 20; index++ {
		firstHash[index], secondHash[index] = byte(index), byte(index)
	}
	firstHash[31], secondHash[31] = 1, 2
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now })
	announce(t, handler, makeGrant(t, privateKey, firstHash, now.Add(time.Hour)), announceQuery(firstHash, "-ARKNET-PEER-0000001", "6881"), "192.0.2.10:50000")
	second := announce(t, handler, makeGrant(t, privateKey, secondHash, now.Add(time.Hour)), announceQuery(secondHash, "-ARKNET-PEER-0000002", "6882"), "192.0.2.11:50001")
	if !bytes.Contains(second.Body.Bytes(), []byte("10:incompletei1e")) || !bytes.Contains(second.Body.Bytes(), []byte("5:peers0:")) {
		t.Fatalf("body = %q", second.Body.Bytes())
	}
}

func TestGrantSubjectOwnsOnlyOnePeerRecord(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 12
	var subject [16]byte
	subject[0] = 1
	token := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subject)
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now })
	first := announce(t, handler, token, announceQuery(hash, "-ARKNET-PEER-0000001", "6881"), "192.0.2.10:50000")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	second := announce(t, handler, token, announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "192.0.2.11:50001")
	if second.Code != http.StatusOK || !bytes.Contains(second.Body.Bytes(), []byte("10:incompletei1e")) {
		t.Fatalf("second status = %d body = %q; want one subject-owned record", second.Code, second.Body.Bytes())
	}
}

func TestDifferentGrantSubjectCannotOverwritePeerID(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 13
	var firstSubject, secondSubject [16]byte
	firstSubject[0], secondSubject[0] = 1, 2
	firstToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), firstSubject)
	secondToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), secondSubject)
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now })
	peerID := "-ARKNET-PEER-0000001"
	announce(t, handler, firstToken, announceQuery(hash, peerID, "6881"), "192.0.2.10:50000")
	response := announce(t, handler, secondToken, announceQuery(hash, peerID, "6999"), "192.0.2.11:50001")
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
}

func TestDifferentGrantSubjectCannotStopPeerID(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 14
	var firstSubject, secondSubject, observerSubject [16]byte
	firstSubject[0], secondSubject[0], observerSubject[0] = 1, 2, 3
	firstToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), firstSubject)
	secondToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), secondSubject)
	observerToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), observerSubject)
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now })
	peerID := "-ARKNET-PEER-0000001"
	announce(t, handler, firstToken, seedQuery(hash, peerID, "6881"), "192.0.2.10:50000") // a seed, so observable
	stopped, _ := url.ParseQuery(announceQuery(hash, peerID, "6999"))
	stopped.Set("event", "stopped")
	response := announce(t, handler, secondToken, stopped.Encode(), "192.0.2.11:50001")
	if response.Code != http.StatusConflict {
		t.Fatalf("stop status = %d, want %d", response.Code, http.StatusConflict)
	}
	observer := announce(t, handler, observerToken, announceQuery(hash, "-ARKNET-PEER-0000003", "6883"), "192.0.2.12:50002")
	if !bytes.Contains(observer.Body.Bytes(), []byte{192, 0, 2, 10, 0x1a, 0xe1}) {
		t.Fatalf("original peer was removed: %q", observer.Body.Bytes())
	}
}

func TestAnnounceRejectsNewPeerWhenSwarmCapacityReached(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	clock := now
	var hash [32]byte
	hash[0] = 11
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return clock })
	var firstToken string
	for index := 0; index < 1024; index++ {
		peerID := fmt.Sprintf("-AR%017d", index)
		token := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(uint64(index+1)))
		if index == 0 {
			firstToken = token
		}
		response := announce(t, handler, token, announceQuery(hash, peerID, strconv.Itoa(6000+index%50000)), "192.0.2.1:50000")
		if response.Code != http.StatusOK {
			t.Fatalf("peer %d status = %d", index, response.Code)
		}
	}
	overflowToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(1025))
	response := announce(t, handler, overflowToken, announceQuery(hash, "-AR99999999999999999", "7000"), "192.0.2.2:50000")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if response.Body.Len() > 256 || !bytes.Contains(response.Body.Bytes(), []byte("failure reason")) {
		t.Fatalf("unbounded or invalid failure body: %q", response.Body.Bytes())
	}
	update := announce(t, handler, firstToken, announceQuery(hash, "-AR00000000000000000", "7001"), "192.0.2.3:50000")
	if update.Code != http.StatusOK {
		t.Fatalf("existing peer update status = %d", update.Code)
	}
	clock = now.Add(time.Minute)
	afterExpiry := announce(t, handler, overflowToken, announceQuery(hash, "-AR88888888888888888", "7002"), "192.0.2.4:50000")
	if afterExpiry.Code != http.StatusOK {
		t.Fatalf("new peer after stale pruning status = %d", afterExpiry.Code)
	}
}

func TestAnnounceCapsNumwantAtFifty(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 10
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now })
	for index := 0; index < 55; index++ {
		peerID := fmt.Sprintf("-ARKNET-PEER-%07d", index)
		remote := fmt.Sprintf("192.0.2.%d:50000", index+1)
		token := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(uint64(index+1)))
		response := announce(t, handler, token, seedQuery(hash, peerID, strconv.Itoa(6000+index)), remote)
		if response.Code != http.StatusOK {
			t.Fatalf("peer %d status = %d", index, response.Code)
		}
	}
	values, _ := url.ParseQuery(announceQuery(hash, "-ARKNET-PEER-9999999", "7000"))
	values.Set("numwant", "200")
	observerToken := makeGrantForSubject(t, privateKey, hash, now.Add(time.Hour), subjectFor(56))
	response := announce(t, handler, observerToken, values.Encode(), "198.51.100.1:50000")
	marker := []byte("5:peers300:")
	if !bytes.Contains(response.Body.Bytes(), marker) {
		t.Fatalf("body does not contain capped 300-byte IPv4 peer list: %q", response.Body.Bytes())
	}
}

func TestAnnounceEnforcesGrantActionsFromRequestLeft(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 15

	tests := map[string]struct {
		actions byte
		left    string
		status  int
	}{
		"correct download succeeds": {actions: grant.ActionDownload, left: "1", status: http.StatusOK},
		"seed only cannot download": {actions: grant.ActionSeed, left: "1", status: http.StatusForbidden},
		"download only cannot seed": {actions: grant.ActionDownload, left: "0", status: http.StatusForbidden},
		"both can download":         {actions: grant.ActionDownload | grant.ActionSeed, left: "1", status: http.StatusOK},
		"both can seed":             {actions: grant.ActionDownload | grant.ActionSeed, left: "0", status: http.StatusOK},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			token := makeGrantForSubjectAndActions(t, privateKey, hash, now.Add(time.Hour), subjectFor(1), testCase.actions)
			values, _ := url.ParseQuery(announceQuery(hash, "-ARKNET-PEER-0000001", "6881"))
			values.Set("left", testCase.left)
			response := announce(t, NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now }), token, values.Encode(), "192.0.2.10:50000")
			if response.Code != testCase.status {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, testCase.status, response.Body.Bytes())
			}
		})
	}
}

func TestStoppedActionDenialOccursBeforeSwarmMutation(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 16
	owner := subjectFor(1)
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now })
	bothToken := makeGrantForSubjectAndActions(t, privateKey, hash, now.Add(time.Hour), owner, grant.ActionDownload|grant.ActionSeed)
	peerID := "-ARKNET-PEER-0000001"
	if response := announce(t, handler, bothToken, announceQuery(hash, peerID, "6881"), "192.0.2.10:50000"); response.Code != http.StatusOK {
		t.Fatalf("register status = %d", response.Code)
	}

	stopped, _ := url.ParseQuery(announceQuery(hash, peerID, "6881"))
	stopped.Set("event", "stopped")
	seedToken := makeGrantForSubjectAndActions(t, privateKey, hash, now.Add(time.Hour), owner, grant.ActionSeed)
	if response := announce(t, handler, seedToken, stopped.Encode(), "192.0.2.10:50000"); response.Code != http.StatusForbidden {
		t.Fatalf("left>0 stop status = %d, want %d", response.Code, http.StatusForbidden)
	}

	stopped.Set("left", "0")
	downloadToken := makeGrantForSubjectAndActions(t, privateKey, hash, now.Add(time.Hour), owner, grant.ActionDownload)
	if response := announce(t, handler, downloadToken, stopped.Encode(), "192.0.2.10:50000"); response.Code != http.StatusForbidden {
		t.Fatalf("left=0 stop status = %d, want %d", response.Code, http.StatusForbidden)
	}

	observerToken := makeGrantForSubjectAndActions(t, privateKey, hash, now.Add(time.Hour), subjectFor(2), grant.ActionDownload)
	observer := announce(t, handler, observerToken, announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "192.0.2.11:50001")
	// The owner (still downloading, so not advertised) must remain registered.
	if !bytes.Contains(observer.Body.Bytes(), []byte("10:incompletei2e")) {
		t.Fatalf("denied stopped announce mutated swarm: %q", observer.Body.Bytes())
	}
}

func TestAnnounceRejectsInvalidStandardOptionalFields(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	token := makeGrant(t, privateKey, hash, now.Add(time.Hour))
	tests := map[string][2]string{
		"negative corrupt": {"corrupt", "-1"},
		"empty key":        {"key", ""},
		"long key":         {"key", string(bytes.Repeat([]byte{'a'}, 65))},
		"binary key":       {"key", "bad\x01key"},
		"no peer id":       {"no_peer_id", "2"},
		"support crypto":   {"supportcrypto", "yes"},
		"redundant":        {"redundant", "1.0"},
	}
	for name, field := range tests {
		t.Run(name, func(t *testing.T) {
			values, _ := url.ParseQuery(announceQuery(hash, "-ARKNET-PEER-0000001", "6881"))
			values.Set(field[0], field[1])
			response := announce(t, NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now }), token, values.Encode(), "192.0.2.1:50000")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d", response.Code)
			}
		})
	}
}

func TestAnnounceRejectsDuplicateOptionalField(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	token := makeGrant(t, privateKey, hash, now.Add(time.Hour))
	query := announceQuery(hash, "-ARKNET-PEER-0000001", "6881") + "&key=deadbeef&key=cafebabe"
	response := announce(t, NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now }), token, query, "192.0.2.1:50000")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", response.Code)
	}
}

// Receiving never implies serving: a Node that is still downloading (left>0,
// download grant) is never advertised, so no peer can request its partial
// pieces while Peer Serving is off. Only seed-authorized complete peers are
// returned, to downloaders and seeds alike.
func TestIncompletePeersAreNeverAdvertised(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	var hash [32]byte
	hash[0] = 21
	handler := NewHandler(publicKey, time.Minute, time.Minute, func() time.Time { return now })
	downloadOnly := func(index uint64) string {
		return makeGrantForSubjectAndActions(t, privateKey, hash, now.Add(time.Hour), subjectFor(index), grant.ActionDownload)
	}
	announce(t, handler, downloadOnly(1), announceQuery(hash, "-ARKNET-PEER-0000001", "6881"), "192.0.2.10:50000")
	second := announce(t, handler, downloadOnly(2), announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "192.0.2.11:50001")
	if want := "d8:completei0e10:incompletei2e8:intervali60e5:peers0:6:peers60:e"; second.Body.String() != want {
		t.Fatalf("downloader advertised to another downloader: %q", second.Body.Bytes())
	}
	seedToken := makeGrantForSubjectAndActions(t, privateKey, hash, now.Add(time.Hour), subjectFor(3), grant.ActionSeed)
	seed := announce(t, handler, seedToken, seedQuery(hash, "-ARKNET-PEER-0000003", "6883"), "192.0.2.12:50002")
	if want := "d8:completei1e10:incompletei2e8:intervali60e5:peers0:6:peers60:e"; seed.Body.String() != want {
		t.Fatalf("downloaders advertised to a seed: %q", seed.Body.Bytes())
	}
	third := announce(t, handler, downloadOnly(1), announceQuery(hash, "-ARKNET-PEER-0000001", "6881"), "192.0.2.10:50000")
	want := append([]byte("d8:completei1e10:incompletei2e8:intervali60e5:peers6:"), []byte{192, 0, 2, 12, 0x1a, 0xe3}...)
	want = append(want, []byte("6:peers60:e")...)
	if !bytes.Equal(third.Body.Bytes(), want) {
		t.Fatalf("downloader must learn only the seed: %q", third.Body.Bytes())
	}
}
