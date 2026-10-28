package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"arknet/core/internal/media"
	"arknet/core/internal/nodes"
	"arknet/core/internal/postgres"
	"arknet/core/internal/store"
)

// Exercise the real PostgreSQL -> Core HTTP path, including node proof and
// approval. Each run owns only its random schema, never developer catalog data.
func TestMediaCatalogPostgresHTTP(t *testing.T) {
	dsn := os.Getenv("ARKNET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ARKNET_TEST_DATABASE_URL for real PostgreSQL catalog HTTP tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("open test database failed")
	}
	defer admin.Close()
	nodeID, err := nodes.NewPublicID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "arknet_test_" + nodeID[5:21]
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	repository, err := postgres.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RegisterEnrollment(ctx, nodeID, version, publicKey); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateMediaObject(ctx, store.Object{
		ObjectType: media.ObjectTypeVideo, Title: "Catalog integration movie", Size: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newHandler(repository, repository))
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	path := "/api/federation/v1/nodes/" + nodeID + "/media"
	get := func(suffix string, want int, result any) {
		t.Helper()
		request := signedRequest(http.MethodGet, server.URL+path+suffix, "", nodeID, privateKey, fmt.Sprint(time.Now().Unix()))
		request.RequestURI = ""
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("catalog response status = %d, want %d", response.StatusCode, want)
		}
		if result != nil {
			if err := json.NewDecoder(response.Body).Decode(result); err != nil {
				t.Fatal(err)
			}
		}
	}
	get("", http.StatusForbidden, nil)
	if _, err := repository.Approve(ctx, nodeID); err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Objects []media.APIObject `json:"objects"`
	}
	get("", http.StatusOK, &catalog)
	if len(catalog.Objects) != 1 || catalog.Objects[0] != created.AsAPIView() {
		t.Fatalf("catalog did not preserve created metadata: %+v", catalog.Objects)
	}
	var object media.APIObject
	get("/"+created.ID, http.StatusOK, &object)
	if object != created.AsAPIView() {
		t.Fatalf("lookup = %+v, want %+v", object, created.AsAPIView())
	}
	removed, err := repository.UpdateMediaObjectState(ctx, created.ID, media.ObjectRemoved)
	if err != nil {
		t.Fatal(err)
	}
	get("", http.StatusOK, &catalog)
	get("/"+created.ID, http.StatusOK, &object)
	if len(catalog.Objects) != 1 || catalog.Objects[0] != removed.AsAPIView() || object != removed.AsAPIView() {
		t.Fatalf("removal tombstone lost: catalog=%+v lookup=%+v", catalog.Objects, object)
	}
	if removed.ID != created.ID || !removed.CreatedAt.Equal(created.CreatedAt) || removed.UpdatedAt.Before(created.UpdatedAt) {
		t.Fatalf("removal changed identity or creation time: %+v", removed)
	}
}
