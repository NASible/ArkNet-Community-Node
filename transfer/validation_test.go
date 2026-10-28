package main

import (
	"arknet/transfer/engine"
	"strings"
	"testing"
	"time"
)

func TestStrictValidation(t *testing.T) {
	h := newHandler(testToken, engine.New(time.Hour, nil))
	valid := addBody(objectID, 1)
	hash := strings.Repeat("d", 64)
	bad := []string{
		"", "null", "[]", "true", "{}", valid + " {}", valid + " null", valid + "x",
		`{"object_id":"` + objectID + `","size":1}`, `{"object_id":"` + objectID + `","size":null,"info_hash_v2":"` + hash + `"}`,
		`{"object_id":"` + objectID + `","size":1.0,"info_hash_v2":"` + hash + `"}`, `{"object_id":"` + objectID + `","size":1e2,"info_hash_v2":"` + hash + `"}`,
		strings.Replace(valid, `"size":1`, `"size":-1`, 1), strings.Replace(valid, `"size":1`, `"size":9223372036854775808`, 1),
		strings.Replace(valid, `"size":1`, `"size":1,"size":1`, 1), strings.Replace(valid, `"size":1`, `"Size":1`, 1),
		strings.Replace(valid, `"info_hash_v2":"`+hash+`"`, `"info_hash_v2":null`, 1), strings.Replace(valid, objectID, "media_"+strings.Repeat("A", 64), 1),
		strings.Replace(valid, hash, "abc", 1), strings.Replace(valid, hash, strings.ToUpper(hash), 1),
	}
	for _, key := range []string{"title", "path", "url", "credentials", "token"} {
		bad = append(bad, strings.TrimSuffix(valid, "}")+`,"`+key+`":"secret"}`)
	}
	bad = append(bad, valid+strings.Repeat(" ", 4096), strings.Replace(valid, objectID, "bad", 1))
	for _, body := range bad {
		if v := request(t, h, "POST", base, body, testToken, 400); v["error"] != "invalid_request" {
			t.Fatal(v)
		}
	}
	request(t, h, "POST", base, valid, testToken, 200)
	for _, body := range []string{"", "null", "[]", `{"x":1}`, "{} {}", "{}" + strings.Repeat(" ", 4096)} {
		request(t, h, "POST", base+"/"+objectID+"/pause", body, testToken, 400)
	}
	request(t, h, "POST", "/api/transfer/v1/mock/transfers/"+objectID+"/complete", "{}", testToken, 404)
	for _, path := range []string{base + "?x=1", base + "?", base + "/bad", base + "/" + objectID + "/", base + "/", base + "//" + objectID, base + "/%6dedia_" + strings.Repeat("a", 64), "/api/transfer/v1/version?token=secret", "/health?x=1"} {
		request(t, h, "GET", path, "", testToken, 400)
	}
}

func TestAuthorizationFieldsAreStrict(t *testing.T) {
	recorder := &authorizationRecordingEngine{Engine: engine.New(time.Hour, nil)}
	h := newHandler(testToken, recorder)
	valid := strings.TrimSuffix(addBody(objectID, 1), "}") + `,"tracker_url":"` + trackerURL + `","web_seed_url":"` + webSeedURL + `","torrent_url":"` + torrentURL + `"}`
	bad := []string{
		strings.Replace(valid, `"web_seed_url":"`+webSeedURL+`"`, `"web_seed_url":null`, 1),
		strings.Replace(valid, `"web_seed_url":"`+webSeedURL+`"`, `"web_seed_url":1`, 1),
		strings.Replace(valid, `"web_seed_url":"`+webSeedURL+`"`, `"web_seed_url":"`+webSeedURL+`","web_seed_url":"`+webSeedURL+`"`, 1),
		strings.Replace(valid, `"web_seed_url"`, `"Web_Seed_URL"`, 1),
		strings.Replace(valid, `"tracker_url":"`+trackerURL+`"`, `"tracker_url":null`, 1),
		strings.Replace(valid, `"tracker_url"`, `"Tracker_URL"`, 1),
		strings.Replace(valid, `"torrent_url":"`+torrentURL+`"`, `"torrent_url":null`, 1),
		strings.Replace(valid, `"torrent_url"`, `"Torrent_URL"`, 1),
		strings.Replace(valid, webSeedURL, strings.Repeat("a", 4097), 1),
	}
	for _, body := range bad {
		if v := request(t, h, "POST", base, body, testToken, 400); v["error"] != "invalid_request" {
			t.Fatal(v)
		}
	}
	request(t, h, "POST", base, valid, testToken, 200)
}
