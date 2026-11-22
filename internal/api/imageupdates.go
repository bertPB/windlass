package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/windlass-dev/windlass/internal/auth"
	"github.com/windlass-dev/windlass/internal/deploy"
)

type imageUpdates struct {
	Enabled   bool               `json:"enabled"`
	LastCheck *deploy.ImageCheck `json:"last_check,omitempty"`
}

func (a *API) imageUpdateState(r *http.Request, name string) (imageUpdates, error) {
	enabled, err := a.Projects.AutoUpdateImages(r.Context(), name)
	if err != nil {
		return imageUpdates{}, err
	}
	out := imageUpdates{Enabled: enabled}
	if c, ok := a.Deploy.LastImageCheck(name); ok {
		out.LastCheck = &c
	}
	return out, nil
}

func (a *API) handleGetImageUpdates(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if _, err := a.Projects.Get(r.Context(), name); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	state, err := a.imageUpdateState(r, name)
	if err != nil {
		a.internalError(w, "read image updates", err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (a *API) handleSetImageUpdates(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if _, err := a.Projects.Get(r.Context(), name); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "enabled is required")
		return
	}
	if err := a.Projects.SetAutoUpdateImages(r.Context(), name, *body.Enabled); err != nil {
		a.internalError(w, "save image updates", err)
		return
	}
	user, _ := auth.UserFrom(r.Context())
	a.Audit.Write(r.Context(), user.ID, "project.image_updates", "project", name, remoteIP(r), imageUpdates{Enabled: *body.Enabled})
	a.handleGetImageUpdates(w, r)
}

func (a *API) handleCheckImageUpdates(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if _, err := a.Projects.Get(r.Context(), name); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	if err := a.Deploy.CheckImagesNow(r.Context(), name); err != nil {
		if errors.Is(err, deploy.ErrImageUpdatesOff) {
			writeError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		a.internalError(w, "queue image check", err)
		return
	}
	user, _ := auth.UserFrom(r.Context())
	a.Audit.Write(r.Context(), user.ID, "project.image_check", "project", name, remoteIP(r), nil)
	state, err := a.imageUpdateState(r, name)
	if err != nil {
		a.internalError(w, "read image updates", err)
		return
	}
	writeJSON(w, http.StatusAccepted, state)
}
