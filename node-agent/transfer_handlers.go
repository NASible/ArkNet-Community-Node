package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

// The Agent authorizes ArkNet objects; the transfer boundary receives only an
// object-scoped work request, never enrollment keys or Core administrative access.
type transferOpt struct {
	service transfer.Service
	media   mediaOpt
	// nodeState reads the persisted Node state; a REVOKED Node starts nothing.
	nodeState func(context.Context) (localstate.State, error)
}

func (transferOpt) isOption() {}

type transferListResponse struct {
	Enabled   bool              `json:"enabled"`
	Available bool              `json:"available"`
	Transfers []transfer.Status `json:"transfers"`
}

func (t transferOpt) list(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		mediaError(w, 400, "invalid_request")
		return
	}
	result := transferListResponse{Enabled: t.service != nil, Transfers: make([]transfer.Status, 0)}
	if t.service == nil {
		mediaJSON(w, 200, result)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	if t.media.repository == nil {
		mediaError(w, 503, "local_media_unavailable")
		return
	}
	selected, err := t.media.repository.ListMediaSelections(ctx)
	if err != nil {
		mediaError(w, 503, "local_media_unavailable")
		return
	}
	statuses, err := t.service.List(ctx)
	if err != nil {
		mediaJSON(w, 200, result)
		return
	}
	allowed := make(map[string]mediaselection.Object, len(selected))
	for _, object := range selected {
		allowed[object.ID] = object
	}
	for _, status := range statuses {
		object, ok := allowed[status.ObjectID]
		if !ok {
			continue
		}
		if status.TotalBytes != object.Size {
			mediaJSON(w, 200, result)
			return
		}
		result.Transfers = append(result.Transfers, status)
	}
	result.Available = true
	mediaJSON(w, 200, result)
}

func (t transferOpt) action(w http.ResponseWriter, r *http.Request) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	action, id := r.PathValue("action"), r.PathValue("id")
	if err != nil || contentType != "application/json" || !validSelectionID(id) || r.URL.RawQuery != "" || r.URL.ForceQuery || (action != "start" && action != "pause" && action != "resume" && action != "cancel") {
		mediaError(w, 400, "invalid_request")
		return
	}
	var body map[string]json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	if decoder.Decode(&body) != nil || body == nil || len(body) != 0 || decoder.Decode(new(any)) != io.EOF {
		mediaError(w, 400, "invalid_request")
		return
	}
	if t.service == nil {
		mediaError(w, 503, "transfer_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 16*time.Second)
	defer cancel()
	if t.media.repository == nil {
		mediaError(w, 503, "local_media_unavailable")
		return
	}
	selected, err := t.media.repository.ListMediaSelections(ctx)
	if err != nil {
		mediaError(w, 503, "local_media_unavailable")
		return
	}
	var selection mediaselection.Object
	found := false
	for _, object := range selected {
		if object.ID == id {
			selection = object
			found = true
			break
		}
	}
	if !found {
		mediaError(w, 404, "media_not_found")
		return
	}
	var request transfer.Request
	if action == "start" || action == "resume" {
		var failure *transferFailure
		request, failure = t.authorizeStart(ctx, selection, action)
		if failure != nil {
			mediaError(w, failure.status, failure.code)
			return
		}
	}
	var status transfer.Status
	switch action {
	case "start":
		status, err = t.service.Add(ctx, request)
	case "pause":
		status, err = t.service.Pause(ctx, id)
	case "resume":
		if _, err = t.service.Refresh(ctx, request); err == nil {
			status, err = t.service.Resume(ctx, id)
		}
	case "cancel":
		status, err = t.service.Cancel(ctx, id)
	}
	switch {
	case errors.Is(err, transfer.ErrNotFound):
		mediaError(w, 404, "transfer_not_found")
	case errors.Is(err, transfer.ErrConflict), errors.Is(err, transfer.ErrInvalidTransition):
		mediaError(w, 409, "transfer_conflict")
	case err != nil:
		mediaError(w, 503, "transfer_unavailable")
	case status.ObjectID != id || status.TotalBytes != selection.Size:
		mediaError(w, 503, "transfer_unavailable")
	default:
		if !status.Mock && action == "start" {
			if err := t.media.repository.TransitionMediaSelection(ctx, id, selection.LocalState, mediaselection.Downloading); err != nil {
				mediaError(w, 503, "local_media_unavailable")
				return
			}
		}
		mediaJSON(w, 200, status)
	}
	// A mock completion is not local availability: never mutate media selections
	// to AVAILABLE or claim a real file was downloaded/verified from these reports.
}

type transferFailure struct {
	status int
	code   string
}

// authorizeStart is the single start/resume authorization path, shared by the
// operator API and automatic media-miss handling. It requires a fresh Core
// catalog (object ACTIVE, size unchanged) and a fresh Core transfer grant.
// Pause/cancel deliberately remain available offline and do not use it.
func (t transferOpt) authorizeStart(ctx context.Context, selection mediaselection.Object, action string) (transfer.Request, *transferFailure) {
	if t.nodeState != nil {
		state, err := t.nodeState(ctx)
		if err != nil {
			return transfer.Request{}, &transferFailure{503, "local_state_unavailable"}
		}
		if state == localstate.Revoked {
			return transfer.Request{}, &transferFailure{409, "node_revoked"}
		}
	}
	if selection.State != catalog.Active || selection.LocalState == mediaselection.RemovedRetained {
		return transfer.Request{}, &transferFailure{409, "media_removed"}
	}
	if action == "start" && selection.LocalState != mediaselection.Queued && selection.LocalState != mediaselection.Error {
		return transfer.Request{}, &transferFailure{409, "transfer_conflict"}
	}
	objects, err := t.media.catalog(ctx)
	if err != nil {
		return transfer.Request{}, &transferFailure{503, "catalog_unavailable"}
	}
	for _, object := range objects {
		if object.ID != selection.ID {
			continue
		}
		if object.State != catalog.Active {
			return transfer.Request{}, &transferFailure{409, "media_removed"}
		}
		if object.Size != selection.Size {
			return transfer.Request{}, &transferFailure{409, "transfer_conflict"}
		}
		request := transfer.Request{ObjectID: selection.ID, Size: selection.Size, InfoHashV2: object.InfoHashV2}
		identity, identityErr := t.media.identity(ctx)
		grant := catalog.TransferGrant{}
		if identityErr == nil {
			grantFn := t.media.grantTransfer
			if grantFn == nil {
				grantFn = catalog.Grant
			}
			grant, identityErr = grantFn(ctx, identity, selection.ID)
		}
		if identityErr != nil {
			return transfer.Request{}, &transferFailure{503, "catalog_unavailable"}
		}
		request.TrackerURL, request.WebSeedURL, request.TorrentURL = grant.TrackerURL, grant.WebSeedURL, grant.TorrentURL
		request.Relays, request.RelayListExpiresAt = t.media.currentRelays(time.Now())
		return request, nil
	}
	return transfer.Request{}, &transferFailure{404, "media_not_found"}
}

// startSelection starts a queued selection exactly as the operator "start"
// action does, including the DOWNLOADING transition for real transfers.
func (t transferOpt) startSelection(ctx context.Context, selection mediaselection.Object) error {
	if t.service == nil || t.media.repository == nil {
		return errors.New("transfer unavailable")
	}
	request, failure := t.authorizeStart(ctx, selection, "start")
	if failure != nil {
		return errors.New(failure.code)
	}
	status, err := t.service.Add(ctx, request)
	if err != nil {
		return err
	}
	if status.ObjectID != selection.ID || status.TotalBytes != selection.Size {
		return errors.New("transfer unavailable")
	}
	if !status.Mock {
		return t.media.repository.TransitionMediaSelection(ctx, selection.ID, selection.LocalState, mediaselection.Downloading)
	}
	return nil
}
