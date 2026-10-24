package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"

	"github.com/Blacklotus89898/Petal/internal/pipeline"
	"github.com/Blacklotus89898/Petal/internal/repos"
)

// catalogTTL bounds how stale a repo's listed pipelines may be. Runs never use
// this cache — checkout always clones the run's exact ref.
const catalogTTL = 2 * time.Minute

// catalogScript fetches only .petal/pipelines of the default ref: shallow,
// blob-less and sparse, so listing a big repo costs a few KB.
const catalogScript = `git clone --quiet --depth 1 --filter=blob:none --sparse --branch "$PETAL_REF" "$PETAL_REPO_URL" /src
cd /src
git sparse-checkout set --no-cone .petal/pipelines`

type catalogEntry struct {
	at    time.Time
	specs []*pipeline.Pipeline
}

// RepoPipelines lists the pipelines in an allowlisted repo's default ref
// (GET /api/pipelines?repo=…), via a throwaway alpine/git container like
// checkout, cached per alias for catalogTTL.
func (s *Steps) RepoPipelines(ctx context.Context, alias string) ([]*pipeline.Pipeline, error) {
	repo, ok := s.repos[alias]
	if !ok {
		return nil, fmt.Errorf("repo %q is not in the allowlist", alias)
	}
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock() // one fetch at a time; listing is rare and cheap
	if e, ok := s.catalog[alias]; ok && time.Since(e.at) < catalogTTL {
		return e.specs, nil
	}
	if s.initErr != nil {
		return nil, fmt.Errorf("docker client: %w", s.initErr)
	}
	specs, err := s.fetchCatalog(ctx, repo)
	if err != nil {
		return nil, err
	}
	if s.catalog == nil {
		s.catalog = map[string]catalogEntry{}
	}
	s.catalog[alias] = catalogEntry{at: time.Now(), specs: specs}
	return specs, nil
}

func (s *Steps) fetchCatalog(ctx context.Context, repo repos.Repo) ([]*pipeline.Pipeline, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// Under dataDir, not os.TempDir: the DinD daemon resolves bind sources in
	// its own mount namespace, where only the shared data volume lines up.
	dir := filepath.Join(s.dataDir, "work", "catalog-"+repo.Alias+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	rc, err := s.cli.ImagePull(ctx, checkoutImage, image.PullOptions{})
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", checkoutImage, err)
	}
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()

	ctr, err := s.cli.ContainerCreate(ctx, &container.Config{
		Image:      checkoutImage,
		Env:        []string{"PETAL_REPO_URL=" + repo.URL, "PETAL_REF=" + repo.DefaultRef},
		Entrypoint: []string{"sh", "-c"},
		Cmd:        []string{"set -eu\n" + catalogScript},
		Labels:     map[string]string{"petal.catalog": repo.Alias},
	}, &container.HostConfig{
		NetworkMode: container.NetworkMode(stepNetworkMode(s.dockerHost)),
		Mounts:      []mount.Mount{{Type: mount.TypeBind, Source: dir, Target: "/src"}},
	}, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	defer func() {
		_ = s.cli.ContainerRemove(context.WithoutCancel(ctx), ctr.ID, container.RemoveOptions{Force: true})
	}()
	if err := s.cli.ContainerStart(ctx, ctr.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	statusCh, errCh := s.cli.ContainerWait(ctx, ctr.ID, container.WaitConditionNotRunning)
	select {
	case st := <-statusCh:
		if st.StatusCode != 0 {
			return nil, fmt.Errorf("fetch %s@%s: git exited %d", repo.Alias, repo.DefaultRef, st.StatusCode)
		}
	case err := <-errCh:
		return nil, fmt.Errorf("wait: %w", err)
	}
	return pipeline.List(dir)
}
