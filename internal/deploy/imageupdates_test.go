package deploy

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/windlass-dev/windlass/internal/agent"
	"github.com/windlass-dev/windlass/internal/projects"
	"github.com/windlass-dev/windlass/internal/store/db"
)

// succeed records a finished deployment for project, as if it had run.
func (e *env) succeed(t *testing.T, projectID int64, status string, mod func(db.Deployment)) db.Deployment {
	t.Helper()
	ctx := context.Background()
	d, err := e.q.CreateDeployment(ctx, db.CreateDeploymentParams{ProjectID: projectID, ProjectID_2: projectID, TriggeredBy: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if mod != nil {
		mod(d)
	}
	if status != "" {
		if err := e.q.FinishDeployment(ctx, db.FinishDeploymentParams{ID: d.ID, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestImageUpdateCheck(t *testing.T) {
	for _, scenario := range []string{"changed", "unchanged", "built", "pinned", "promotion", "active", "latest failed", "never deployed", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			if err := e.deploy.projects.SetAutoUpdateImages(ctx, "app", scenario != "disabled"); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "never deployed":
			case "active":
				e.succeed(t, e.project.ID, "succeeded", nil)
				e.succeed(t, e.project.ID, "", nil)
			case "latest failed":
				e.succeed(t, e.project.ID, "succeeded", nil)
				e.succeed(t, e.project.ID, "failed", nil)
			case "pinned":
				e.succeed(t, e.project.ID, "succeeded", nil)
				e.succeed(t, e.project.ID, "succeeded", func(d db.Deployment) {
					if _, err := e.sqlDB.Exec("UPDATE deployments SET rollback_of = ? WHERE id = ?", d.ID-1, d.ID); err != nil {
						t.Fatal(err)
					}
				})
			case "promotion":
				e.succeed(t, e.project.ID, "succeeded", func(d db.Deployment) {
					if _, err := e.sqlDB.Exec("UPDATE deployments SET promoted_from = ? WHERE id = ?", d.ID, d.ID); err != nil {
						t.Fatal(err)
					}
				})
			default:
				e.succeed(t, e.project.ID, "succeeded", nil)
			}
			e.agent.Hashes["app"] = map[string]string{"web": "h1"}
			e.agent.Containers = []agent.Container{{ID: "c1", Name: "app-web-1", ComposeProject: "app", ComposeService: "web", ConfigHash: "h1", ImageID: "old"}}
			e.agent.ImageIDs["nginx:alpine"] = "new"
			if scenario == "unchanged" {
				e.agent.ImageIDs["nginx:alpine"] = "old"
			}
			if scenario == "built" {
				e.agent.Resolved["app"] = agent.ResolvedConfig{Services: map[string]agent.ResolvedService{
					"web": {Image: "nginx:alpine", Build: true},
				}}
			}
			before, err := e.q.LatestDeploymentID(ctx, e.project.ID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				t.Fatal(err)
			}

			if err := e.deploy.checkImageUpdates(ctx, "app"); err != nil {
				t.Fatal(err)
			}

			after, err := e.q.LatestDeploymentID(ctx, e.project.ID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				t.Fatal(err)
			}
			if created, want := after != before, scenario == "changed"; created != want {
				t.Fatalf("deployment created = %v, want %v", created, want)
			}
			pulled := slices.Contains(e.agent.Calls, "compose.pull(app)")
			if want := scenario == "changed" || scenario == "unchanged" || scenario == "built"; pulled != want {
				t.Fatalf("pulled = %v, want %v", pulled, want)
			}
			if scenario == "changed" {
				d, err := e.q.GetDeploymentByID(ctx, after)
				if err != nil || d.TriggeredBy != "schedule" {
					t.Fatalf("deployment = %+v, error = %v", d, err)
				}
				// A scheduled update of staging must not promote over live.
				var queued string
				if err := e.sqlDB.QueryRow("SELECT payload FROM jobs WHERE type = ?", JobType).Scan(&queued); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(queued, `"no_promote":true`) {
					t.Fatalf("scheduled deployment payload %s can promote", queued)
				}
				if final := e.runUntilFinished(t, d.ID); final.Status != "succeeded" {
					t.Fatalf("scheduled deploy = %s: %s", final.Status, final.Error.String)
				}
			}
		})
	}
}

// TestImageUpdatePinsLiveCommit: a scheduled update of a Git project must not
// deploy commits pushed since the last deploy, only newer images.
func TestImageUpdatePinsLiveCommit(t *testing.T) {
	const live, pushed = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	for _, recorded := range []bool{true, false} {
		e := newEnv(t)
		ctx := context.Background()
		p, err := e.deploy.projects.Create(ctx, projects.CreateReq{
			Name: "site", Source: "git", GitRepo: "https://example.com/site.git", Branch: "main",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.deploy.projects.SetAutoUpdateImages(ctx, "site", true); err != nil {
			t.Fatal(err)
		}
		last := e.succeed(t, p.ID, "succeeded", func(d db.Deployment) {
			if !recorded {
				return
			}
			if err := e.q.SetDeploymentCommit(ctx, db.SetDeploymentCommitParams{GitCommit: sql.NullString{String: live, Valid: true}, ID: d.ID}); err != nil {
				t.Fatal(err)
			}
		})
		e.agent.GitCommit = pushed
		e.agent.Resolved["site"] = agent.ResolvedConfig{Services: map[string]agent.ResolvedService{"web": {Image: "nginx:alpine"}}}
		e.agent.Hashes["site"] = map[string]string{"web": "h1"}
		e.agent.Containers = []agent.Container{{ID: "c1", ComposeProject: "site", ComposeService: "web", ConfigHash: "h1", ImageID: "old"}}
		e.agent.ImageIDs["nginx:alpine"] = "new"
		e.agent.Statuses["site"] = []agent.ServiceStatus{{Service: "web", Name: "site-web-1", State: "running"}}

		if err := e.deploy.checkImageUpdates(ctx, "site"); err != nil {
			t.Fatal(err)
		}
		latest, err := e.q.LatestDeploymentID(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !recorded {
			// A project that was manual when it last deployed has no live
			// commit to pin, so it is left alone.
			if latest != last.ID {
				t.Fatal("scheduled a deployment with no live commit to pin")
			}
			continue
		}
		final := e.runUntilFinished(t, latest)
		if final.Status != "succeeded" || final.GitCommit.String != live {
			t.Fatalf("scheduled deploy = %s at %q, want succeeded at the live commit", final.Status, final.GitCommit.String)
		}
	}
}

// TestImageUpdateTick: only opted-in projects get a check, once per interval.
func TestImageUpdateTick(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.deploy.projects.Create(ctx, projects.CreateReq{Name: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := e.deploy.projects.SetAutoUpdateImages(ctx, "app", true); err != nil {
		t.Fatal(err)
	}
	last := map[string]time.Time{}
	e.deploy.imageUpdatesTick(ctx, time.Hour, last)
	e.deploy.imageUpdatesTick(ctx, time.Hour, last)

	var types []string
	rows, err := e.sqlDB.Query("SELECT type, payload FROM jobs ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, payload string
		if err := rows.Scan(&typ, &payload); err != nil {
			t.Fatal(err)
		}
		types = append(types, typ+" "+payload)
	}
	if want := []string{imageUpdateJob + ` {"project":"app"}`}; !slices.Equal(types, want) {
		t.Fatalf("jobs = %q, want %q", types, want)
	}
}
