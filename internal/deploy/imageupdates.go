package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// imageUpdateJob runs one project's image check on the job runner. The
// runner has one worker, so the check's pull never overlaps a deployment:
// a pull landing between a deployment's recorded digests and its up would
// start images the deployment never recorded.
const imageUpdateJob = "image-update"

type imageUpdatePayload struct {
	Project string `json:"project"`
}

// RunImageUpdates queues an image check for each opted-in project once per
// interval until ctx ends.
func (s *Service) RunImageUpdates(ctx context.Context, interval time.Duration) {
	// Give startup recovery time to resume interrupted deployments before pulling.
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	last := map[string]time.Time{}
	s.imageUpdatesTick(ctx, interval, last)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.imageUpdatesTick(ctx, interval, last)
		}
	}
}

func (s *Service) imageUpdatesTick(ctx context.Context, interval time.Duration, last map[string]time.Time) {
	list, err := s.projects.List(ctx)
	if err != nil {
		s.logger.Warn("list projects for image updates", "error", err)
		return
	}
	for _, p := range list {
		if ctx.Err() != nil {
			return
		}
		if time.Since(last[p.Name]) < interval {
			continue
		}
		enabled, err := s.projects.AutoUpdateImages(ctx, p.Name)
		if err != nil {
			s.logger.Warn("read image update setting", "project", p.Name, "error", err)
			continue
		}
		if !enabled {
			continue
		}
		last[p.Name] = time.Now()
		if _, err := s.runner.Enqueue(ctx, imageUpdateJob, imageUpdatePayload{Project: p.Name}); err != nil {
			s.logger.Warn("queue image update check", "project", p.Name, "error", err)
		}
	}
}

func (s *Service) runImageUpdateJob(ctx context.Context, raw json.RawMessage, _ string, _ func(string) error) error {
	var pl imageUpdatePayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return err
	}
	// A failed check waits for the next interval rather than the runner's
	// retries: the usual cause, an unreachable registry, will not have cleared.
	if err := s.checkImageUpdates(ctx, pl.Project); err != nil {
		s.logger.Warn("check image updates", "project", pl.Project, "error", err)
	}
	return nil
}

func (s *Service) checkImageUpdates(ctx context.Context, project string) error {
	p, err := s.projects.Get(ctx, project)
	if err != nil {
		return err
	}
	// Read again here: the job may have waited behind deployments, or a
	// restart, after the setting was turned off.
	if enabled, err := s.projects.AutoUpdateImages(ctx, project); err != nil || !enabled {
		return err
	}
	last, err := s.q.LatestSucceededDeployment(ctx, p.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Only a project whose newest deployment succeeded is known to run what
	// that deployment recorded. After a failed one, which may have started
	// some services, an update would put back the older code.
	latest, err := s.q.LatestDeploymentID(ctx, p.ID)
	if err != nil {
		return err
	}
	if latest != last.ID {
		s.logger.Info("image update skipped: the latest deployment did not succeed", "project", project)
		return nil
	}
	if pinned(last) {
		s.logger.Info("image update skipped for pinned deployment", "project", project)
		return nil
	}
	// Without the commit that is live, sync would check out the branch head
	// and ship whatever was pushed since.
	if p.Source == "git" && p.GitRepo.Valid && !last.GitCommit.Valid {
		s.logger.Info("image update skipped: no Git commit recorded for the live deployment", "project", project)
		return nil
	}
	if s.registry != nil {
		if err := s.registry.Apply(ctx, s.agent.Docker()); err != nil {
			s.logger.Warn("image update registry login", "project", project, "error", err)
		}
	}
	if err := s.agent.Compose().Pull(ctx, project, nil); err != nil {
		return err
	}
	drift, err := s.Drift(ctx, project)
	if err != nil {
		return err
	}
	cfg, err := s.agent.Compose().Config(ctx, project)
	if err != nil {
		return err
	}
	// A built service's image only changes when it is built, so a tag it
	// shares with a pulled image is not news here.
	changed := false
	for _, svc := range drift.Services {
		if svc.ImageChanged && !cfg.Services[svc.Service].Build {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	// NoPromote: a scheduled update of staging must not replace what live
	// runs, which may be a rollback someone chose.
	_, err = s.deploy(ctx, project, "schedule", payload{Project: project, Commit: last.GitCommit.String, NoPromote: true})
	if errors.Is(err, ErrDeployInProgress) {
		return nil
	}
	return err
}
