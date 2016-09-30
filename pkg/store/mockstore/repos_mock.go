// generated by gen-mocks; DO NOT EDIT

package mockstore

import (
	"context"

	"sourcegraph.com/sourcegraph/sourcegraph/api/sourcegraph"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/store"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/vcs"
)

type Repos struct {
	Get_            func(ctx context.Context, repo int32) (*sourcegraph.Repo, error)
	GetByURI_       func(ctx context.Context, repo string) (*sourcegraph.Repo, error)
	List_           func(v0 context.Context, v1 *store.RepoListOp) ([]*sourcegraph.Repo, error)
	Search_         func(v0 context.Context, v1 string) ([]*sourcegraph.RepoSearchResult, error)
	Create_         func(v0 context.Context, v1 *sourcegraph.Repo) (int32, error)
	Update_         func(v0 context.Context, v1 store.RepoUpdate) error
	InternalUpdate_ func(ctx context.Context, repo int32, op store.InternalRepoUpdate) error
	Delete_         func(ctx context.Context, repo int32) error
}

func (s *Repos) Get(ctx context.Context, repo int32) (*sourcegraph.Repo, error) {
	return s.Get_(ctx, repo)
}

func (s *Repos) GetByURI(ctx context.Context, repo string) (*sourcegraph.Repo, error) {
	return s.GetByURI_(ctx, repo)
}

func (s *Repos) List(v0 context.Context, v1 *store.RepoListOp) ([]*sourcegraph.Repo, error) {
	return s.List_(v0, v1)
}

func (s *Repos) Search(v0 context.Context, v1 string) ([]*sourcegraph.RepoSearchResult, error) {
	return s.Search_(v0, v1)
}

func (s *Repos) Create(v0 context.Context, v1 *sourcegraph.Repo) (int32, error) {
	return s.Create_(v0, v1)
}

func (s *Repos) Update(v0 context.Context, v1 store.RepoUpdate) error { return s.Update_(v0, v1) }

func (s *Repos) InternalUpdate(ctx context.Context, repo int32, op store.InternalRepoUpdate) error {
	return s.InternalUpdate_(ctx, repo, op)
}

func (s *Repos) Delete(ctx context.Context, repo int32) error { return s.Delete_(ctx, repo) }

var _ store.Repos = (*Repos)(nil)

type RepoConfigs struct {
	Get_    func(ctx context.Context, repo int32) (*sourcegraph.RepoConfig, error)
	Update_ func(ctx context.Context, repo int32, conf sourcegraph.RepoConfig) error
}

func (s *RepoConfigs) Get(ctx context.Context, repo int32) (*sourcegraph.RepoConfig, error) {
	return s.Get_(ctx, repo)
}

func (s *RepoConfigs) Update(ctx context.Context, repo int32, conf sourcegraph.RepoConfig) error {
	return s.Update_(ctx, repo, conf)
}

var _ store.RepoConfigs = (*RepoConfigs)(nil)

type RepoStatuses struct {
	GetCombined_ func(ctx context.Context, repo int32, commitID string) (*sourcegraph.CombinedStatus, error)
	GetCoverage_ func(ctx context.Context) (*sourcegraph.RepoStatusList, error)
	Create_      func(ctx context.Context, repo int32, commitID string, status *sourcegraph.RepoStatus) error
}

func (s *RepoStatuses) GetCombined(ctx context.Context, repo int32, commitID string) (*sourcegraph.CombinedStatus, error) {
	return s.GetCombined_(ctx, repo, commitID)
}

func (s *RepoStatuses) GetCoverage(ctx context.Context) (*sourcegraph.RepoStatusList, error) {
	return s.GetCoverage_(ctx)
}

func (s *RepoStatuses) Create(ctx context.Context, repo int32, commitID string, status *sourcegraph.RepoStatus) error {
	return s.Create_(ctx, repo, commitID, status)
}

var _ store.RepoStatuses = (*RepoStatuses)(nil)

type RepoVCS struct {
	Open_  func(ctx context.Context, repo int32) (vcs.Repository, error)
	Clone_ func(ctx context.Context, repo int32, info *store.CloneInfo) error
}

func (s *RepoVCS) Open(ctx context.Context, repo int32) (vcs.Repository, error) {
	return s.Open_(ctx, repo)
}

func (s *RepoVCS) Clone(ctx context.Context, repo int32, info *store.CloneInfo) error {
	return s.Clone_(ctx, repo, info)
}

var _ store.RepoVCS = (*RepoVCS)(nil)
