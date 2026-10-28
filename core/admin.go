package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"arknet/core/internal/nodes"
	"arknet/core/internal/store"
	"arknet/core/internal/users"
)

type administrativeNode struct {
	NodeID           string                 `json:"node_id"`
	EnrollmentStatus nodes.EnrollmentStatus `json:"enrollment_status"`
	TrustStatus      nodes.TrustStatus      `json:"trust_status"`
	Version          string                 `json:"version"`
	CreatedAt        time.Time              `json:"created_at"`
}

func administrativeView(node nodes.Node) administrativeNode {
	return administrativeNode{node.PublicID, node.EnrollmentStatus, node.TrustStatus, node.SoftwareVersion, node.CreatedAt}
}

// withAdministration keeps the entire administrative namespace behind one
// replaceable authorizer, including unknown routes and unsupported methods.
func withAdministration(public http.Handler, repository nodes.AdministrationRepository, authorizer adminAuthorizer) http.Handler {
	admin := http.NewServeMux()
	metadata, _ := repository.(nodes.MetadataRepository)
	admin.HandleFunc("GET /api/admin/v1/nodes/{node_id}", administrativeMetadataHandler(metadata))
	userAdministration, _ := repository.(users.Repository)
	registerUserAdministration(admin, userAdministration)
	mediaAdministration, _ := repository.(store.MediaCatalogRepository)
	registerMediaAdministration(admin, mediaAdministration)
	admin.HandleFunc("GET /api/admin/v1/nodes/pending", func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			adminError(w, 400, "invalid pagination")
			return
		}
		for key, values := range query {
			if (key != "limit" && key != "cursor") || len(values) != 1 {
				adminError(w, 400, "invalid pagination")
				return
			}
		}
		limit := 50
		if values, ok := query["limit"]; ok {
			limit, err = strconv.Atoi(values[0])
			if err != nil || limit < 1 || limit > 100 {
				adminError(w, 400, "invalid pagination")
				return
			}
		}
		cursor := query.Get("cursor")
		if cursor != "" && !nodes.ValidPublicID(cursor) {
			adminError(w, 400, "invalid pagination")
			return
		}
		if repository == nil {
			adminError(w, 503, "administration unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		rows, err := repository.ListPending(ctx, cursor, limit)
		if err != nil {
			administrativeRepositoryError(w, err)
			return
		}
		next := ""
		if len(rows) > limit {
			rows = rows[:limit]
			next = rows[len(rows)-1].PublicID
		}
		views := make([]administrativeNode, 0, len(rows))
		for _, row := range rows {
			views = append(views, administrativeView(row))
		}
		writeAdminJSON(w, 200, struct {
			Nodes      []administrativeNode `json:"nodes"`
			NextCursor string               `json:"next_cursor"`
		}{views, next})
	})
	for _, action := range []string{"approve", "reject", "revoke"} {
		admin.HandleFunc("POST /api/admin/v1/nodes/{node_id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("node_id")
			if !nodes.ValidPublicID(id) {
				adminError(w, 400, "invalid node ID")
				return
			}
			if r.URL.RawQuery != "" || r.URL.ForceQuery {
				adminError(w, 400, "action does not accept parameters")
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
			if err != nil || len(body) != 0 {
				adminError(w, 400, "action requires an empty body")
				return
			}
			if repository == nil {
				adminError(w, 503, "administration unavailable")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			var node nodes.Node
			switch action {
			case "approve":
				node, err = repository.Approve(ctx, id)
			case "reject":
				node, err = repository.Reject(ctx, id)
			default:
				node, err = repository.Revoke(ctx, id)
			}
			if err != nil {
				administrativeRepositoryError(w, err)
				return
			}
			writeAdminJSON(w, 200, struct {
				NodeID           string                 `json:"node_id"`
				EnrollmentStatus nodes.EnrollmentStatus `json:"enrollment_status"`
				TrustStatus      nodes.TrustStatus      `json:"trust_status"`
			}{node.PublicID, node.EnrollmentStatus, node.TrustStatus})
		})
	}
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", "Authorization")
		if authorizer == nil {
			http.NotFound(w, r)
			return
		}
		if !authorizer.Authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="arknet-core-admin"`)
			adminError(w, 401, "administrative authorization required")
			return
		}
		admin.ServeHTTP(w, r)
	})
	mux := http.NewServeMux()
	mux.Handle("/api/admin/", protected)
	mux.Handle("/", public)
	return mux
}

func writeAdminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func adminError(w http.ResponseWriter, status int, message string) {
	writeAdminJSON(w, status, struct {
		Error string `json:"error"`
	}{message})
}
func administrativeRepositoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, nodes.ErrInvalidPublicID), errors.Is(err, nodes.ErrInvalidPage):
		adminError(w, 400, "invalid request")
	case errors.Is(err, nodes.ErrNotFound):
		adminError(w, 404, "node not found")
	case errors.Is(err, nodes.ErrEnrollmentConflict):
		adminError(w, 409, "node is not eligible for a pending enrollment decision")
	default:
		adminError(w, 503, "administration unavailable")
	}
}
