// Integration-only proof helper; never prints or exports credential material.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/postgres"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := postgres.Open(ctx, os.Getenv("ARKNET_TEST_DATABASE_URL"))
	if err != nil {
		panic("probe database unavailable")
	}
	defer store.Close()
	identity, err := store.EnrollmentIdentity(ctx)
	if err != nil {
		panic("probe identity unavailable")
	}
	target := os.Getenv("ARKNET_TEST_TARGET_NODE_ID")
	if target == "" {
		target = identity.NodeID
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(os.Getenv("ARKNET_TEST_CORE_URL"), "/")+"/api/federation/v1/nodes/"+target+"/enrollment", nil)
	if err != nil {
		panic("probe request invalid")
	}
	if err := enrollment.SignRequest(req, target, identity.PrivateSeed, nil); err != nil {
		panic("probe signature failed")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		panic("probe transport failed")
	}
	defer response.Body.Close()
	expected := 200
	if target != identity.NodeID {
		expected = 401
	}
	if response.StatusCode != expected {
		panic(fmt.Sprintf("probe authentication status %d, expected %d", response.StatusCode, expected))
	}
	fmt.Printf("PASS: node-bound credential authentication returned %d\n", expected)
}
