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

// ImageCheck is the outcome of a project's latest image check. Result is
// queued, no_change, deployed, skipped or failed. Outcomes are kept in
// memory only: they say what happened since Windlass started, and a
// deployment an update started is in the project's history either way.
type ImageCheck struct {
	At         time.Time `json:"at"`
	Result     string    `json:"result"`
	Detail     string    `json:"detail,omitempty"`
	Deployment int64     `json:"deployment,omitempty"`
}

// ErrImageUpdatesOff refuses a manual check of a project that has not
// opted in, since the check itself would skip it.
var ErrImageUpdatesOff = errors.New("automatic image updates are off for this project")

// LastImageCheck returns the project's latest image check, if there has
// been one since Windlass started.
func (s *Service) LastImageCheck(project string) (ImageCheck, bool) {
	s.checksMu.Lock()
	defer s.checksMu.Unlock()
	c, ok := s.checks[project]
	return c, ok
}

func (s *Service) recordImageCheck(project string, c ImageCheck) {
	c.At = time.Now().UTC()
	s.checksMu.Lock()
	defer s.checksMu.Unlock()
	if s.checks == nil {
		s.checks = map[string]ImageCheck{}
	}
	s.checks[project] = c
}

// CheckImagesNow queues an image check for project ahead of its schedule.
// A check already queued is not queued twice.
func (s *Service) CheckImagesNow(ctx context.Context, project string) error {
	enabled, err := s.projects.AutoUpdateImages(ctx, project)
	if err != nil {
		return err
	}
	if !enabled {
		return ErrImageUpdatesOff
	}
	return s.queueImageCheck(ctx, project)
}

func (s *Service) queueImageCheck(ctx context.Context, project string) error {
	if c, ok := s.LastImageCheck(project); ok && c.Result == "queued" {
		return nil
	}
	if _, err := s.runner.Enqueue(ctx, imageUpdateJob, imageUpdatePayload{Project: project}); err != nil {
		return err
	}
	s.recordImageCheck(project, ImageCheck{Result: "queued"})
	return nil
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
		if err := s.queueImageCheck(ctx, p.Name); err != nil {
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
	c, err := s.checkImageUpdates(ctx, pl.Project)
	if err != nil {
		s.logger.Warn("check image updates", "project", pl.Project, "error", err)
		c = ImageCheck{Result: "failed", Detail: err.Error()}
	} else if c.Result == "skipped" {
		s.logger.Info("image update skipped", "project", pl.Project, "reason", c.Detail)
	}
	s.recordImageCheck(pl.Project, c)
	return nil
}

func skipped(reason string) ImageCheck { return ImageCheck{Result: "skipped", Detail: reason} }

func (s *Service) checkImageUpdates(ctx context.Context, project string) (ImageCheck, error) {
	p, err := s.projects.Get(ctx, project)
	if err != nil {
		return ImageCheck{}, err
	}
	// Read again here: the job may have waited behind deployments, or a
	// restart, after the setting was turned off.
	enabled, err := s.projects.AutoUpdateImages(ctx, project)
	if err != nil {
		return ImageCheck{}, err
	}
	if !enabled {
		return skipped("automatic image updates were turned off"), nil
	}
	last, err := s.q.LatestSucceededDeployment(ctx, p.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return skipped("the project has not deployed successfully yet"), nil
	}
	if err != nil {
		return ImageCheck{}, err
	}
	// Only a project whose newest deployment succeeded is known to run what
	// that deployment recorded. After a failed one, which may have started
	// some services, an update would put back the older code.
	latest, err := s.q.LatestDeploymentID(ctx, p.ID)
	if err != nil {
		return ImageCheck{}, err
	}
	if latest != last.ID {
		return skipped("the latest deployment is running or did not succeed"), nil
	}
	if pinned(last) {
		return skipped("a rollback or promotion is live; deploy to resume updates"), nil
	}
	// Without the commit that is live, sync would check out the branch head
	// and ship whatever was pushed since.
	if p.Source == "git" && p.GitRepo.Valid && !last.GitCommit.Valid {
		return skipped("the live deployment recorded no Git commit; deploy once to resume updates"), nil
	}
	if s.registry != nil {
		if err := s.registry.Apply(ctx, s.agent.Docker()); err != nil {
			s.logger.Warn("image update registry login", "project", project, "error", err)
		}
	}
	if err := s.agent.Compose().Pull(ctx, project, nil); err != nil {
		return ImageCheck{}, err
	}
	drift, err := s.Drift(ctx, project)
	if err != nil {
		return ImageCheck{}, err
	}
	cfg, err := s.agent.Compose().Config(ctx, project)
	if err != nil {
		return ImageCheck{}, err
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
		return ImageCheck{Result: "no_change"}, nil
	}
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	// NoPromote: a scheduled update of staging must not replace what live
	// runs, which may be a rollback someone chose.
	d, err := s.deploy(ctx, project, "schedule", payload{Project: project, Commit: last.GitCommit.String, NoPromote: true})
	if errors.Is(err, ErrDeployInProgress) {
		return skipped("a deployment started during the check"), nil
	}
	if err != nil {
		return ImageCheck{}, err
	}
	return ImageCheck{Result: "deployed", Deployment: d.Number}, nil
}
