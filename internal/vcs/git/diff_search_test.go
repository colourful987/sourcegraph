	"github.com/sourcegraph/sourcegraph/internal/vcs/git/gittest"
	gitCommands := []string{
	}
	tests := map[string]struct {
		repo gitserver.Repo
		want map[*git.RawLogDiffSearchOptions][]*git.LogCommitSearchResult
	}{
		"git cmd": {
			repo: gittest.MakeGitRepository(t, gitCommands...),
			want: map[*git.RawLogDiffSearchOptions][]*git.LogCommitSearchResult{
				{
					Query: git.TextSearchOptions{Pattern: "root"},
					Diff:  true,
				}: {
					{
						Commit: git.Commit{
							ID:        "b9b2349a02271ca96e82c70f384812f9c62c26ab",
							Author:    git.Signature{Name: "a", Email: "a@a.com", Date: gittest.MustParseTime(time.RFC3339, "2006-01-02T15:04:06Z")},
							Committer: &git.Signature{Name: "a", Email: "a@a.com", Date: gittest.MustParseTime(time.RFC3339, "2006-01-02T15:04:06Z")},
							Message:   "branch1",
							Parents:   []api.CommitID{"ce72ece27fd5c8180cfbc1c412021d32fd1cda0d"},
						},
						Refs:       []string{"refs/heads/branch1"},
						SourceRefs: []string{"refs/heads/branch2"},
						Diff:       &git.Diff{Raw: "diff --git a/f b/f\nindex d8649da..1193ff4 100644\n--- a/f\n+++ b/f\n@@ -1,1 +1,1 @@\n-root\n+branch1\n"},
					},
					{
						Commit: git.Commit{
							ID:        "ce72ece27fd5c8180cfbc1c412021d32fd1cda0d",
							Author:    git.Signature{Name: "a", Email: "a@a.com", Date: gittest.MustParseTime(time.RFC3339, "2006-01-02T15:04:05Z")},
							Committer: &git.Signature{Name: "a", Email: "a@a.com", Date: gittest.MustParseTime(time.RFC3339, "2006-01-02T15:04:05Z")},
							Message:   "root",
						},
						Refs:       []string{"refs/heads/master", "refs/tags/mytag"},
						SourceRefs: []string{"refs/heads/branch2"},
						Diff:       &git.Diff{Raw: "diff --git a/f b/f\nnew file mode 100644\nindex 0000000..d8649da\n--- /dev/null\n+++ b/f\n@@ -0,0 +1,1 @@\n+root\n"},
					},
				},

				// Without query
				{
					Query: git.TextSearchOptions{Pattern: ""},
					Args:  []string{"--grep=branch1|root", "--extended-regexp"},
				}: {
					{
						Commit: git.Commit{
							ID:        "b9b2349a02271ca96e82c70f384812f9c62c26ab",
							Author:    git.Signature{Name: "a", Email: "a@a.com", Date: gittest.MustParseTime(time.RFC3339, "2006-01-02T15:04:06Z")},
							Committer: &git.Signature{Name: "a", Email: "a@a.com", Date: gittest.MustParseTime(time.RFC3339, "2006-01-02T15:04:06Z")},
							Message:   "branch1",
							Parents:   []api.CommitID{"ce72ece27fd5c8180cfbc1c412021d32fd1cda0d"},
						},
						Refs:       []string{"refs/heads/branch1"},
						SourceRefs: []string{"refs/heads/branch2"},
					},
					{
						Commit: git.Commit{
							ID:        "ce72ece27fd5c8180cfbc1c412021d32fd1cda0d",
							Author:    git.Signature{Name: "a", Email: "a@a.com", Date: gittest.MustParseTime(time.RFC3339, "2006-01-02T15:04:05Z")},
							Committer: &git.Signature{Name: "a", Email: "a@a.com", Date: gittest.MustParseTime(time.RFC3339, "2006-01-02T15:04:05Z")},
							Message:   "root",
						},
						Refs:       []string{"refs/heads/master", "refs/tags/mytag"},
						SourceRefs: []string{"refs/heads/branch2"},
					},
				},

				// With path exclude/include filters
				{
					Paths: git.PathOptions{
						IncludePatterns: []string{"g"},
						ExcludePattern:  "f",
						IsRegExp:        true,
					},
				}: nil, // empty
	}
	for label, test := range tests {
		for opt, want := range test.want {
			results, complete, err := git.RawLogDiffSearch(ctx, test.repo, *opt)
				t.Errorf("%s: %+v: %s", label, *opt, err)
				continue
				t.Errorf("%s: !complete", label)
			if !reflect.DeepEqual(results, want) {
				t.Errorf("%s: %+v: got %+v, want %+v", label, *opt, gittest.AsJSON(results), gittest.AsJSON(want))
		}
			repo: gittest.MakeGitRepository(t, gitCommands...),
				t.Errorf("%s: %+v: got %+v, want %+v", label, *opt, gittest.AsJSON(results), gittest.AsJSON(want))