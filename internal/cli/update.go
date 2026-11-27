// File update.go implements `flowsk update` for checking whether a newer public
// release of Flowsk is published on GitHub (`github.com/mindmain/flowsk`).
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mindmain/flowsk/internal/version"
	"github.com/spf13/cobra"
)

type githubReleasePayload struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
	Prerelease  bool   `json:"prerelease"`
}

type githubTagPayload struct {
	Name string `json:"name"`
}

// newUpdateCommand constructs `flowsk update`.
func newUpdateCommand(opts *GlobalOptions, deps FormatterDependency) *cobra.Command {
	var formatter OutputFormatter
	if deps != nil {
		formatter = deps.OutputFormatter()
	}

	var (
		repo    string
		apiBase string
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Check GitHub for newer Flowsk releases and print upgrade instructions",
		Long: `Queries GitHub Releases (and Tags fallback) for 'mindmain/flowsk' to check
whether a newer version of Flowsk has been published compared to the running binary.

Upgrade options when a newer release is available:
  - Download a prebuilt release binary from GitHub Releases
  - Run 'go install github.com/mindmain/flowsk/cmd/flowsk@<tag>'
  - Run 'task install' from an updated local source checkout

If the repository is not yet public or has no published releases yet, 'flowsk update'
reports 'unpublished' cleanly with exit code 0.`,
		Example: `  flowsk update
  flowsk update --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("update context canceled: %w", err)
			}
			cleanRepo := strings.Trim(strings.TrimSpace(repo), "/")
			if cleanRepo == "" || !strings.Contains(cleanRepo, "/") {
				return fmt.Errorf("invalid --repo %q: expected 'owner/name'", repo)
			}
			if timeout <= 0 {
				timeout = 5 * time.Second
			}

			view, err := checkGitHubUpdate(ctx, strings.TrimRight(strings.TrimSpace(apiBase), "/"), cleanRepo, timeout)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), formatter, opts, view)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&repo, "repo", version.DefaultGitHubRepo, "GitHub 'owner/repo' to inspect for releases")
	flags.DurationVar(&timeout, "timeout", 5*time.Second, "HTTP request timeout when querying GitHub")
	flags.StringVar(&apiBase, "api-base", "https://api.github.com", "GitHub REST API base URL")
	_ = flags.MarkHidden("api-base")
	return cmd
}

func checkGitHubUpdate(ctx context.Context, apiBase, repo string, timeout time.Duration) (UpdateCheckView, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{Timeout: timeout}
	current := version.NormalizedTag()

	releaseURL := fmt.Sprintf("%s/repos/%s/releases/latest", apiBase, repo)
	relBody, statusCode, err := fetchGitHubJSON(reqCtx, client, releaseURL)
	if err != nil {
		return UpdateCheckView{}, fmt.Errorf("query GitHub releases (%s): %w", repo, err)
	}

	var latestTag, htmlURL, publishedAt string
	switch statusCode {
	case http.StatusOK:
		var rel githubReleasePayload
		if uErr := json.Unmarshal(relBody, &rel); uErr != nil {
			return UpdateCheckView{}, fmt.Errorf("decode GitHub release response: %w", uErr)
		}
		latestTag = strings.TrimSpace(rel.TagName)
		htmlURL = strings.TrimSpace(rel.HTMLURL)
		publishedAt = strings.TrimSpace(rel.PublishedAt)

	case http.StatusNotFound:
		// Either no GitHub Release object exists yet (tags-only) or repository is not public yet.
		tagsURL := fmt.Sprintf("%s/repos/%s/tags?per_page=1", apiBase, repo)
		tagBody, tagStatus, tErr := fetchGitHubJSON(reqCtx, client, tagsURL)
		if tErr != nil {
			return UpdateCheckView{}, fmt.Errorf("query GitHub tags (%s): %w", repo, tErr)
		}
		if tagStatus == http.StatusNotFound {
			return UpdateCheckView{
				Repo:           repo,
				CurrentVersion: current,
				Status:         "unpublished",
				Message:        fmt.Sprintf("No public GitHub release found for github.com/%s yet (running %s).", repo, current),
			}, nil
		}
		if tagStatus != http.StatusOK {
			return UpdateCheckView{}, fmt.Errorf("unexpected GitHub tags HTTP status %d", tagStatus)
		}
		var tags []githubTagPayload
		if uErr := json.Unmarshal(tagBody, &tags); uErr != nil {
			return UpdateCheckView{}, fmt.Errorf("decode GitHub tags response: %w", uErr)
		}
		if len(tags) == 0 || strings.TrimSpace(tags[0].Name) == "" {
			return UpdateCheckView{
				Repo:           repo,
				CurrentVersion: current,
				Status:         "unpublished",
				Message:        fmt.Sprintf("Repository github.com/%s has no published release tags yet (running %s).", repo, current),
			}, nil
		}
		latestTag = strings.TrimSpace(tags[0].Name)
		htmlURL = fmt.Sprintf("https://github.com/%s/releases/tag/%s", repo, latestTag)

	default:
		return UpdateCheckView{}, fmt.Errorf("unexpected GitHub API HTTP status %d", statusCode)
	}

	if !strings.HasPrefix(latestTag, "v") && !strings.HasPrefix(latestTag, "V") {
		latestTag = "v" + latestTag
	}
	if htmlURL == "" {
		htmlURL = fmt.Sprintf("https://github.com/%s/releases/tag/%s", repo, latestTag)
	}

	cmpVal, ok := version.CompareSemver(current, latestTag)
	if !ok {
		if strings.EqualFold(current, latestTag) {
			cmpVal = 0
		} else {
			cmpVal = -1
		}
	}

	switch {
	case cmpVal < 0:
		upgradeCmd := fmt.Sprintf("go install github.com/%s/cmd/flowsk@%s", repo, latestTag)
		return UpdateCheckView{
			Repo:            repo,
			CurrentVersion:  current,
			LatestVersion:   latestTag,
			Status:          "update_available",
			UpdateAvailable: true,
			ReleaseURL:      htmlURL,
			PublishedAt:     publishedAt,
			UpgradeCommand:  upgradeCmd,
			Message:         fmt.Sprintf("Newer Flowsk release %s is available (current: %s).", latestTag, current),
		}, nil
	case cmpVal > 0:
		return UpdateCheckView{
			Repo:            repo,
			CurrentVersion:  current,
			LatestVersion:   latestTag,
			Status:          "ahead",
			UpdateAvailable: false,
			ReleaseURL:      htmlURL,
			PublishedAt:     publishedAt,
			Message:         fmt.Sprintf("Running local build %s ahead of latest published release %s.", current, latestTag),
		}, nil
	default:
		return UpdateCheckView{
			Repo:            repo,
			CurrentVersion:  current,
			LatestVersion:   latestTag,
			Status:          "up_to_date",
			UpdateAvailable: false,
			ReleaseURL:      htmlURL,
			PublishedAt:     publishedAt,
			Message:         fmt.Sprintf("Flowsk %s is up to date.", current),
		}, nil
	}
}

func fetchGitHubJSON(ctx context.Context, client *http.Client, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "flowsk-cli/"+version.NormalizedTag())

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func renderHumanUpdateCheck(w io.Writer, v UpdateCheckView) error {
	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		tuiTitleBadgeStyle.Render("FLOWSK UPDATE"),
		"  ",
		tuiSubtitleStyle.Render("github.com/"+v.Repo),
	)

	var b strings.Builder
	b.WriteString(header + "\n\n")

	switch v.Status {
	case "update_available":
		fmt.Fprintf(&b, "  %s  %s → %s\n", tuiWarnStyle.Render("↻ UPDATE AVAILABLE"), v.CurrentVersion, tuiOKStyle.Render(v.LatestVersion))
		if v.ReleaseURL != "" {
			fmt.Fprintf(&b, "  Release:  %s\n", tuiMutedStyle.Render(v.ReleaseURL))
		}
		if v.UpgradeCommand != "" {
			releaseHint := fmt.Sprintf("https://github.com/%s/releases/latest", v.Repo)
			if v.ReleaseURL != "" {
				releaseHint = v.ReleaseURL
			}
			fmt.Fprintf(&b, "\n  Upgrade options:\n    %s\n    %s (from source checkout)\n    Download binary: %s\n  Then refresh AI skills:\n    %s",
				tuiInfoStyle.Render(v.UpgradeCommand),
				tuiInfoStyle.Render("task install"),
				tuiMutedStyle.Render(releaseHint),
				tuiInfoStyle.Render("flowsk install --yes"),
			)
		}
	case "up_to_date":
		fmt.Fprintf(&b, "  %s  %s\n", tuiOKStyle.Render("✓ UP TO DATE"), v.Message)
	case "ahead":
		fmt.Fprintf(&b, "  %s  %s\n", tuiInfoStyle.Render("• LOCAL AHEAD"), v.Message)
	default:
		fmt.Fprintf(&b, "  %s  %s", tuiInfoStyle.Render("• UNPUBLISHED"), v.Message)
	}

	_, err := io.WriteString(w, tuiCardStyle.Render(strings.TrimRight(b.String(), "\n"))+"\n")
	return err
}
