package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

// finalizeMP4 imports only the transfer engine's exact opaque-ID output. It
// never accepts a caller-supplied path or filename.
func finalizeMP4(stagingDir, mediaDir, objectID string, expectedSize int64) (string, error) {
	if !validSelectionID(objectID) || expectedSize < 12 || stagingDir == "" || mediaDir == "" {
		return "", errors.New("invalid media import")
	}
	destination := filepath.Join(mediaDir, objectID+".mp4")
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() || info.Size() != expectedSize {
			return "", errors.New("unsafe existing media object")
		}
		file, err := os.Open(destination)
		if err != nil {
			return "", err
		}
		defer file.Close()
		if err := validateMP4(file, info, expectedSize); err != nil {
			return "", err
		}
		return destination, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	source := filepath.Join(stagingDir, objectID, objectID)
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return "", errors.New("invalid staged media object")
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("staged media changed during validation")
	}
	if err := validateMP4(input, opened, expectedSize); err != nil {
		return "", err
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(mediaDir, ".arknet-import-*")
	if err != nil {
		return "", err
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", err
	}
	written, err := io.Copy(temporary, io.LimitReader(input, expectedSize+1))
	if err != nil || written != expectedSize {
		return "", errors.New("incomplete media import")
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return "", err
	}
	committed = true
	if err := os.Remove(source); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	_ = os.Remove(filepath.Dir(source))
	return destination, nil
}

func validateMP4(file *os.File, info os.FileInfo, expectedSize int64) error {
	if info == nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return errors.New("invalid media size")
	}
	header := make([]byte, 12)
	if _, err := io.ReadFull(file, header); err != nil {
		return errors.New("invalid mp4 header")
	}
	if string(header[4:8]) != "ftyp" {
		return errors.New("unsupported media type")
	}
	boxSize := int64(uint32(header[0])<<24 | uint32(header[1])<<16 | uint32(header[2])<<8 | uint32(header[3]))
	if boxSize != 0 && (boxSize < 8 || boxSize > expectedSize) {
		return errors.New("invalid mp4 box")
	}
	return nil
}

type replicaReporter func(context.Context, enrollment.Identity, string, int64, string) error

type transferReconciler struct {
	service         transfer.Service
	media           mediaOpt
	stagingDir      string
	mediaDir        string
	refreshJellyfin func(context.Context) error
	reportReplica   replicaReporter
	// nodeState reads the persisted Node state. On terminal REVOKED every
	// active transfer is cancelled and nothing is asked of Core.
	nodeState func(context.Context) (localstate.State, error)
}

func (r transferReconciler) once(ctx context.Context) error {
	if r.service == nil || r.media.repository == nil {
		return nil
	}
	if r.nodeState != nil {
		state, err := r.nodeState(ctx)
		if err != nil {
			return err
		}
		if state == localstate.Revoked {
			return r.cancelForRevocation(ctx)
		}
	}
	selections, err := r.media.repository.ListMediaSelections(ctx)
	if err != nil {
		return err
	}
	statuses, err := r.service.List(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]mediaselection.Object, len(selections))
	for _, selection := range selections {
		byID[selection.ID] = selection
	}
	for _, status := range statuses {
		selection, ok := byID[status.ObjectID]
		if !ok || status.Mock || status.TotalBytes != selection.Size {
			continue
		}
		if selection.State == catalog.Removed {
			if status.State == transfer.Downloading || status.State == transfer.Paused {
				if _, err := r.service.Cancel(ctx, selection.ID); err != nil && !errors.Is(err, transfer.ErrNotFound) && !errors.Is(err, transfer.ErrInvalidTransition) {
					return err
				}
			}
			continue
		}
		switch status.State {
		case transfer.VerificationFailed:
			if selection.LocalState == mediaselection.Downloading {
				if err := r.media.repository.TransitionMediaSelection(ctx, selection.ID, mediaselection.Downloading, mediaselection.Error); err != nil {
					return err
				}
			}
		case transfer.Paused, transfer.Downloading:
			if selection.LocalState != mediaselection.Downloading {
				continue
			}
			request, err := r.authorizedRequest(ctx, selection)
			if err != nil {
				return err
			}
			if _, err := r.service.Refresh(ctx, request); err != nil {
				return err
			}
			if status.State == transfer.Paused {
				if _, err := r.service.Resume(ctx, selection.ID); err != nil && !errors.Is(err, transfer.ErrInvalidTransition) {
					return err
				}
			}
		case transfer.Completed:
			if status.CompletedBytes != status.TotalBytes || (selection.LocalState != mediaselection.Downloading && selection.LocalState != mediaselection.Available) {
				continue
			}
			request, identity, err := r.authorizedObject(ctx, selection)
			if err != nil {
				return err
			}
			if _, err := finalizeMP4(r.stagingDir, r.mediaDir, selection.ID, selection.Size); err != nil {
				if selection.LocalState == mediaselection.Downloading {
					_ = r.media.repository.TransitionMediaSelection(ctx, selection.ID, mediaselection.Downloading, mediaselection.Error)
				}
				return err
			}
			if selection.LocalState == mediaselection.Downloading {
				if err := r.media.repository.TransitionMediaSelection(ctx, selection.ID, mediaselection.Downloading, mediaselection.Available); err != nil {
					return err
				}
			}
			if r.refreshJellyfin != nil {
				if err := r.refreshJellyfin(ctx); err != nil {
					return err
				}
			}
			if r.reportReplica != nil {
				if err := r.reportReplica(ctx, identity, selection.ID, selection.Size, request.InfoHashV2); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// cancelForRevocation stops every active or paused transfer, including ones
// without a matching selection. Verified local media (AVAILABLE files) is
// never touched; interrupted selections become ERROR. Finished transfers are
// left alone: they move no bytes.
func (r transferReconciler) cancelForRevocation(ctx context.Context) error {
	statuses, err := r.service.List(ctx)
	if err != nil {
		return err
	}
	selections, selectionErr := r.media.repository.ListMediaSelections(ctx)
	errs := []error{selectionErr}
	byID := make(map[string]mediaselection.Object, len(selections))
	for _, selection := range selections {
		byID[selection.ID] = selection
	}
	for _, status := range statuses {
		if status.State != transfer.Downloading && status.State != transfer.Paused {
			continue
		}
		if _, err := r.service.Cancel(ctx, status.ObjectID); err != nil && !errors.Is(err, transfer.ErrNotFound) && !errors.Is(err, transfer.ErrInvalidTransition) {
			errs = append(errs, err)
			continue
		}
		if selection, ok := byID[status.ObjectID]; ok && selection.LocalState == mediaselection.Downloading {
			errs = append(errs, r.media.repository.TransitionMediaSelection(ctx, selection.ID, mediaselection.Downloading, mediaselection.Error))
		}
	}
	return errors.Join(errs...)
}

func (r transferReconciler) authorizedObject(ctx context.Context, selection mediaselection.Object) (transfer.Request, enrollment.Identity, error) {
	identity, err := r.media.identity(ctx)
	if err != nil {
		return transfer.Request{}, enrollment.Identity{}, err
	}
	objects, err := r.media.catalog(ctx)
	if err != nil {
		return transfer.Request{}, enrollment.Identity{}, err
	}
	for _, object := range objects {
		if object.ID == selection.ID && object.State == catalog.Active && object.Size == selection.Size && len(object.InfoHashV2) == 64 {
			return transfer.Request{ObjectID: object.ID, Size: object.Size, InfoHashV2: object.InfoHashV2}, identity, nil
		}
	}
	return transfer.Request{}, enrollment.Identity{}, errors.New("authorized media object unavailable")
}

func (r transferReconciler) authorizedRequest(ctx context.Context, selection mediaselection.Object) (transfer.Request, error) {
	request, identity, err := r.authorizedObject(ctx, selection)
	if err != nil {
		return transfer.Request{}, err
	}
	grantFn := r.media.grantTransfer
	if grantFn == nil {
		grantFn = catalog.Grant
	}
	grant, err := grantFn(ctx, identity, selection.ID)
	if err != nil {
		return transfer.Request{}, err
	}
	request.TrackerURL, request.WebSeedURL, request.TorrentURL = grant.TrackerURL, grant.WebSeedURL, grant.TorrentURL
	request.Relays, request.RelayListExpiresAt = r.media.currentRelays(time.Now())
	return request, nil
}

type intervalReconciler interface {
	once(context.Context) error
}

func runTransferReconciler(ctx context.Context, interval time.Duration, reconciler transferReconciler, onError func(error), wake <-chan struct{}) <-chan struct{} {
	return runReconciler(ctx, interval, "transfer reconciliation", reconciler, onError, wake)
}

// runReconciler runs one reconcile pass immediately, then once per interval
// and whenever wake fires (a nil wake never fires).
func runReconciler(ctx context.Context, interval time.Duration, name string, reconciler intervalReconciler, onError func(error), wake <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if interval <= 0 {
			interval = 2 * time.Minute
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := reconciler.once(callCtx)
			cancel()
			if err != nil && onError != nil {
				onError(fmt.Errorf("%s: %w", name, err))
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-wake:
			}
		}
	}()
	return done
}
