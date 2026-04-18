package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/downloader"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"

	"github.com/spf13/cobra"
)

var (
	blogName         string
	postURL          string
	withMD           bool
	withComments     bool
	downloadExternal bool
	forceDownload    bool
	syncMode         bool
	checkMedia       bool
	checkFilesFlag   bool
	dirFormat        string
	numWorkers       int
)

var downloadCmd = &cobra.Command{
	Use:   "download",
	Short: "Download posts from a boosty blog",
	RunE:  runDownload,
}

func init() {
	downloadCmd.Flags().StringVar(&blogName, "blog", "", "blog name to download all posts")
	downloadCmd.Flags().StringVar(&postURL, "url", "", "single post URL to download")
	downloadCmd.Flags().BoolVar(&withMD, "md", false, "generate markdown file")
	downloadCmd.Flags().BoolVar(&withComments, "comments", false, "download comments")
	downloadCmd.Flags().BoolVar(&downloadExternal, "download-external", false, "download external videos via yt-dlp")
	downloadCmd.Flags().BoolVar(&forceDownload, "force", false, "re-download all posts ignoring state")
	downloadCmd.Flags().BoolVar(&syncMode, "sync", false, "sync mode: check for updates, show diff, confirm before applying")
	downloadCmd.Flags().BoolVar(&checkMedia, "check-media", false, "with --sync: also validate video file sizes via HEAD requests")
	downloadCmd.Flags().BoolVar(&checkFilesFlag, "check-files", false, "with --sync: verify post.json, comments.json, post.md exist on disk")
	downloadCmd.Flags().StringVar(&dirFormat, "format", parser.DefaultFormat, "directory name format: {date}, {date:FORMAT}, {title}, {id}")
	downloadCmd.Flags().IntVar(&numWorkers, "workers", 1, "number of concurrent downloads")
	rootCmd.AddCommand(downloadCmd)
}

var boostyURLRe = regexp.MustCompile(`boosty\.to/([^/]+)/posts/([^/?#]+)`)

func newClient() (*boosty.Client, error) {
	tokens, err := boosty.LoadTokens(authPath)
	if err != nil {
		return nil, err
	}
	c := boosty.NewClient(tokens, authPath)
	c.Log = &stdLogger{}
	return c, nil
}

func runDownload(cmd *cobra.Command, args []string) error {
	if blogName == "" && postURL == "" {
		return fmt.Errorf("specify --blog or --url")
	}

	c, err := newClient()
	if err != nil {
		return err
	}

	if postURL != "" {
		matches := boostyURLRe.FindStringSubmatch(postURL)
		if matches == nil {
			return fmt.Errorf("invalid boosty URL: %s", postURL)
		}
		blog := matches[1]
		postID := matches[2]

		url := boosty.PostURL(blog, postID)
		var post boosty.Post
		if err := c.GetJSON(url, &post); err != nil {
			return fmt.Errorf("fetch post: %w", err)
		}
		_, err := savePost(c, blog, &post)
		return err
	}

	if syncMode {
		return syncBlog(c, blogName)
	}

	return downloadAllPosts(c, blogName)
}

// postStateEntry builds a state.PostEntry from a post.
// HasComments / HasMd reflect the current run's flags; for updates of an
// existing post use postStateEntryPreserving to carry over prior flags.
func postStateEntry(post *boosty.Post, dirName string) state.PostEntry {
	tier := ""
	if post.SubscriptionLevel != nil {
		tier = post.SubscriptionLevel.Name
	}
	return state.PostEntry{
		Title:         post.Title,
		DirName:       dirName,
		UpdatedAt:     post.UpdatedAt,
		CommentsCount: post.Count.Comments,
		Price:         post.Price,
		Tier:          tier,
		HasComments:   withComments,
		HasMd:         withMD,
	}
}

// postStateEntryPreserving builds a state entry from an updated post while
// carrying HasComments/HasMd flags from the prior entry, so that re-saving
// a post without re-downloading comments/md does not "forget" those files.
func postStateEntryPreserving(post *boosty.Post, dirName string, old state.PostEntry) state.PostEntry {
	entry := postStateEntry(post, dirName)
	entry.HasComments = old.HasComments
	entry.HasMd = old.HasMd || withMD
	return entry
}

// writeJSON marshals v with indent and writes it to path (0644) atomically.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	return state.WriteFileAtomic(path, data, 0644)
}

// writePostMarkdown generates markdown for a post and writes it to dir/post.md
// atomically. Returns an error so callers can avoid persisting HasMd=true on
// failure.
func writePostMarkdown(post *boosty.Post, parsed parser.ParsedContent, dir string) error {
	md := parser.GenerateMarkdown(post, parsed)
	return state.WriteFileAtomic(filepath.Join(dir, "post.md"), []byte(md), 0644)
}

// resolveDirName returns a directory name (relative to blogDir) safe to use for
// the given postID. If base is free or already holds this same post, base is
// returned. If base is occupied by a different post's post.json, the post ID
// is appended as a suffix so the caller never silently overwrites a sibling.
func resolveDirName(blogDir, postID, base string) string {
	target := filepath.Join(blogDir, base)
	data, err := os.ReadFile(filepath.Join(target, "post.json"))
	if err != nil {
		return base
	}
	var existing struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &existing); err != nil || existing.ID == "" || existing.ID == postID {
		return base
	}
	suffix := postID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return base + "_" + suffix
}

// savePost downloads a post's full content into the output directory.
// Returns the directory name actually used (which may include a collision
// suffix) so the caller can record it in state.
func savePost(c *boosty.Client, blog string, post *boosty.Post) (string, error) {
	if !post.HasAccess {
		c.Log.Printf("  skipping (no access): %s", post.Title)
		return "", nil
	}

	blogDir := filepath.Join(outputDir, blog)
	dirName := parser.FormatDirName(dirFormat, post.Title, post.PublishTime, post.ID)
	dirName = resolveDirName(blogDir, post.ID, dirName)
	dir := filepath.Join(blogDir, dirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	// Save post.json
	if err := writeJSON(filepath.Join(dir, "post.json"), post); err != nil {
		return "", err
	}
	c.Log.Printf("  saved post.json: %s", post.Title)

	// Parse content blocks
	parsed := parser.ParseBlocks(post.Data)

	// Download media
	if err := downloader.DownloadMedia(c, parsed.Media, dir); err != nil {
		return "", err
	}

	// External videos
	if downloadExternal {
		if err := downloader.DownloadExternal(c.Log, parsed.Media, dir); err != nil {
			c.Log.Printf("  warning: external download error: %v", err)
		}
	}

	// Markdown
	if withMD {
		if err := writePostMarkdown(post, parsed, dir); err != nil {
			return "", err
		}
		c.Log.Printf("  saved post.md")
	}

	// Comments
	if withComments {
		if err := downloadComments(c, blog, post.ID, dir); err != nil {
			c.Log.Printf("  warning: comments error: %v", err)
		}
	}

	return dirName, nil
}

func downloadComments(c *boosty.Client, blog, postID, dir string) error {
	var allComments []boosty.Comment
	for comment, err := range c.FetchComments(blog, postID, 20) {
		if err != nil {
			return err
		}
		allComments = append(allComments, comment)
	}

	if err := writeJSON(filepath.Join(dir, "comments.json"), allComments); err != nil {
		return err
	}
	c.Log.Printf("  saved comments.json (%d comments)", len(allComments))
	return nil
}

type postJob struct {
	num  int
	post boosty.Post
}

func downloadAllPosts(c *boosty.Client, blog string) error {
	c.Log.Printf("Fetching all posts from %s...", blog)

	blogDir := filepath.Join(outputDir, blog)
	if err := os.MkdirAll(blogDir, 0755); err != nil {
		return err
	}

	st := state.Load(blogDir)
	var stMu sync.Mutex

	// Collect posts to download
	var jobs []postJob
	total := 0
	skippedState := 0

	for post, err := range c.FetchPosts(blog, 10) {
		if err != nil {
			return err
		}
		total++

		if !post.HasAccess {
			c.Log.Printf("  [%d] skipping (locked): %s", total, post.Title)
			continue
		}

		if !forceDownload && st.Has(post.ID) {
			skippedState++
			continue
		}

		jobs = append(jobs, postJob{num: total, post: post})
	}

	if len(jobs) == 0 {
		c.Log.Printf("Done. %d total, 0 new, %d already synced.", total, skippedState)
		return nil
	}

	c.Log.Printf("Found %d posts to download (workers: %d)", len(jobs), numWorkers)

	// Download with worker pool: clamp to [1, len(jobs)]
	workers := max(1, min(numWorkers, len(jobs)))

	var downloaded int
	jobCh := make(chan postJob, len(jobs))
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for job := range jobCh {
				c.Log.Printf("  [%d] %s", job.num, job.post.Title)
				dirName, err := savePost(c, blog, &job.post)
				if err != nil {
					c.Log.Printf("  error: %v", err)
					continue
				}

				stMu.Lock()
				st.Add(job.post.ID, postStateEntry(&job.post, dirName))
				if err := st.Save(); err != nil {
					c.Log.Printf("  warning: failed to save state: %v", err)
				}
				downloaded++
				stMu.Unlock()
			}
		})
	}

	wg.Wait()

	c.Log.Printf("Done. %d total, %d downloaded, %d already synced.", total, downloaded, skippedState)
	return nil
}

// --- Sync mode ---

type syncAction string

const (
	actionNew           syncAction = "NEW"
	actionUnlocked      syncAction = "UNLOCKED"
	actionLockedNew     syncAction = "LOCKED_NEW"
	actionLocked        syncAction = "LOCKED"
	actionUpdated       syncAction = "UPDATED"
	actionComments      syncAction = "COMMENTS"
	actionVideoMismatch syncAction = "VIDEO_MISMATCH"
	actionFilesMissing  syncAction = "FILES_MISSING"
	actionNoChange      syncAction = "NO_CHANGE"
)

type syncItem struct {
	Action  syncAction
	Post    boosty.Post
	DirName string
	Detail  string
}

func syncBlog(c *boosty.Client, blog string) error {
	c.Log.Printf("Syncing %s...", blog)

	blogDir := filepath.Join(outputDir, blog)
	if err := os.MkdirAll(blogDir, 0755); err != nil {
		return err
	}

	st := state.Load(blogDir)

	// Phase 1+2: Fetch all posts and classify
	var items []syncItem

	for post, err := range c.FetchPosts(blog, 20) {
		if err != nil {
			return err
		}

		dirName := parser.FormatDirName(dirFormat, post.Title, post.PublishTime, post.ID)
		existing, inState := st.Get(post.ID)

		if !inState {
			if post.HasAccess {
				items = append(items, syncItem{Action: actionNew, Post: post, DirName: dirName})
			} else {
				items = append(items, syncItem{Action: actionLockedNew, Post: post, DirName: dirName})
			}
			continue
		}

		if !post.HasAccess {
			if !existing.Locked {
				items = append(items, syncItem{Action: actionLocked, Post: post, DirName: existing.DirName, Detail: "was accessible, now locked"})
			}
			continue
		}

		if existing.Locked {
			items = append(items, syncItem{Action: actionUnlocked, Post: post, DirName: dirName, Detail: "was locked, now accessible"})
			continue
		}

		// State entries written before UpdatedAt was added to the schema have
		// UpdatedAt == 0; treating that as an edit would flag every such post as
		// UPDATED on first sync after upgrade. Require a known previous value.
		if existing.UpdatedAt != 0 && post.UpdatedAt != existing.UpdatedAt {
			items = append(items, syncItem{Action: actionUpdated, Post: post, DirName: existing.DirName, Detail: "post edited"})
			continue
		}

		// Opportunistic backfill: record UpdatedAt for legacy entries so future
		// syncs can detect real edits. Persists on the next state.Save().
		if existing.UpdatedAt == 0 && post.UpdatedAt != 0 {
			existing.UpdatedAt = post.UpdatedAt
			st.Posts[post.ID] = existing
		}

		if post.Count.Comments != existing.CommentsCount {
			items = append(items, syncItem{
				Action:  actionComments,
				Post:    post,
				DirName: existing.DirName,
				Detail:  fmt.Sprintf("comments: %d → %d", existing.CommentsCount, post.Count.Comments),
			})
			continue
		}

		items = append(items, syncItem{Action: actionNoChange, Post: post, DirName: existing.DirName})
	}

	// Phase 2.5: Check media sizes if requested
	if checkMedia {
		// Collect indices to check. A video mismatch overrides the current
		// classification (e.g. a COMMENTS item with a missing video gets
		// promoted to VIDEO_MISMATCH so the video is refetched).
		var jobs []int
		for i, item := range items {
			switch item.Action {
			case actionNoChange, actionUpdated, actionComments:
			default:
				continue
			}
			if !item.Post.HasAccess {
				continue
			}
			jobs = append(jobs, i)
		}

		workers := max(1, min(numWorkers, len(jobs)))
		c.Log.Printf("Checking media sizes (%d posts, %d workers)...", len(jobs), workers)

		jobCh := make(chan int, len(jobs))
		for _, j := range jobs {
			jobCh <- j
		}
		close(jobCh)

		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				for idx := range jobCh {
					post := items[idx].Post
					dir := filepath.Join(blogDir, items[idx].DirName)
					mismatch := checkVideoSizes(c, blog, &post, dir)
					if mismatch != "" {
						// Each worker owns distinct indices, so writing
						// items[idx] concurrently is safe.
						items[idx].Action = actionVideoMismatch
						items[idx].Detail = mismatch
					}
				}
			})
		}
		wg.Wait()
	}

	// Phase 2.6: Check files on disk if requested
	if checkFilesFlag {
		c.Log.Printf("Checking files on disk...")
		for i, item := range items {
			if item.Action != actionNoChange && item.Action != actionUpdated && item.Action != actionComments {
				continue
			}
			existing, ok := st.Get(item.Post.ID)
			if !ok {
				continue
			}
			missing := checkMissingFiles(existing, filepath.Join(blogDir, item.DirName))
			if missing != "" {
				items[i].Action = actionFilesMissing
				items[i].Detail = missing
			}
		}
	}

	// Phase 3: Show diff
	counts := map[syncAction]int{}
	for _, item := range items {
		counts[item.Action]++
	}

	// Show actionable items
	for _, item := range items {
		if item.Action == actionNoChange {
			continue
		}
		label := string(item.Action)
		detail := ""
		if item.Detail != "" {
			detail = " (" + item.Detail + ")"
		}
		c.Log.Printf("  [%s] %s%s", label, item.Post.Title, detail)
	}

	c.Log.Printf("")
	c.Log.Printf("Sync summary:")
	if n := counts[actionNew]; n > 0 {
		c.Log.Printf("  %d new posts", n)
	}
	if n := counts[actionUnlocked]; n > 0 {
		c.Log.Printf("  %d unlocked posts", n)
	}
	if n := counts[actionUpdated]; n > 0 {
		c.Log.Printf("  %d updated posts", n)
	}
	if n := counts[actionComments]; n > 0 {
		c.Log.Printf("  %d comments updated", n)
	}
	if n := counts[actionVideoMismatch]; n > 0 {
		c.Log.Printf("  %d video size mismatches", n)
	}
	if n := counts[actionFilesMissing]; n > 0 {
		c.Log.Printf("  %d files missing on disk", n)
	}
	if n := counts[actionLocked]; n > 0 {
		c.Log.Printf("  %d locked (data preserved)", n)
	}
	if n := counts[actionLockedNew]; n > 0 {
		c.Log.Printf("  %d locked (no access)", n)
	}
	c.Log.Printf("  %d no changes", counts[actionNoChange])

	actionable := counts[actionNew] + counts[actionUnlocked] + counts[actionUpdated] +
		counts[actionComments] + counts[actionVideoMismatch] + counts[actionFilesMissing] + counts[actionLocked]
	if actionable == 0 {
		c.Log.Printf("Everything up to date.")
		return nil
	}

	// Confirm
	c.Log.Printf("")
	fmt.Print("Apply changes? [y/N] ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		c.Log.Printf("Cancelled.")
		return nil
	}

	// Phase 4: Apply
	c.Log.Printf("Applying...")
	for _, item := range items {
		switch item.Action {
		case actionNew, actionUnlocked:
			c.Log.Printf("  downloading: %s", item.Post.Title)
			dirName, err := savePost(c, blog, &item.Post)
			if err != nil {
				c.Log.Printf("  error: %v", err)
				continue
			}
			st.Add(item.Post.ID, postStateEntry(&item.Post, dirName))
			if err := st.Save(); err != nil {
				c.Log.Printf("  warning: failed to save state: %v", err)
			}

		case actionUpdated:
			c.Log.Printf("  updating: %s", item.Post.Title)
			dir := filepath.Join(blogDir, item.DirName)
			// Re-fetch full post for fresh data
			var fullPost boosty.Post
			if err := c.GetJSON(boosty.PostURL(blog, item.Post.ID), &fullPost); err != nil {
				c.Log.Printf("  error fetching post: %v", err)
				continue
			}
			if err := writeJSON(filepath.Join(dir, "post.json"), fullPost); err != nil {
				c.Log.Printf("  error writing post.json: %v", err)
				continue
			}
			mdWritten := false
			if withMD {
				parsed := parser.ParseBlocks(fullPost.Data)
				if err := writePostMarkdown(&fullPost, parsed, dir); err != nil {
					c.Log.Printf("  error writing post.md: %v", err)
				} else {
					mdWritten = true
				}
			}
			entry := postStateEntryPreserving(&fullPost, item.DirName, st.Posts[item.Post.ID])
			if withMD && !mdWritten {
				// Don't claim md exists when this run failed to write it; keep
				// whatever the prior entry said.
				entry.HasMd = st.Posts[item.Post.ID].HasMd
			}
			st.Add(item.Post.ID, entry)
			if err := st.Save(); err != nil {
				c.Log.Printf("  warning: failed to save state: %v", err)
			}

		case actionComments:
			c.Log.Printf("  updating comments: %s", item.Post.Title)
			dir := filepath.Join(blogDir, item.DirName)
			if err := downloadComments(c, blog, item.Post.ID, dir); err != nil {
				c.Log.Printf("  error: %v", err)
				continue
			}
			entry := st.Posts[item.Post.ID]
			entry.CommentsCount = item.Post.Count.Comments
			entry.HasComments = true
			st.Add(item.Post.ID, entry)
			if err := st.Save(); err != nil {
				c.Log.Printf("  warning: failed to save state: %v", err)
			}

		case actionVideoMismatch:
			c.Log.Printf("  re-downloading video: %s", item.Post.Title)
			dir := filepath.Join(blogDir, item.DirName)
			// Re-fetch post for fresh video URLs
			var fullPost boosty.Post
			if err := c.GetJSON(boosty.PostURL(blog, item.Post.ID), &fullPost); err != nil {
				c.Log.Printf("  error: %v", err)
				continue
			}
			parsed := parser.ParseBlocks(fullPost.Data)
			// Delete existing videos so DownloadFile's integrity check doesn't
			// skip them as already-present. If remove fails we must abort this
			// video: leaving the old file means the redownload is a no-op.
			removeFailed := false
			for _, m := range parsed.Media {
				if m.Type != "video" {
					continue
				}
				p := filepath.Join(dir, m.Filename)
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					c.Log.Printf("  error: cannot remove %s: %v (skipping redownload)", p, err)
					removeFailed = true
				}
			}
			if removeFailed {
				continue
			}
			if err := downloader.DownloadMedia(c, parsed.Media, dir); err != nil {
				c.Log.Printf("  error re-downloading media: %v", err)
			}
			st.Add(item.Post.ID, postStateEntryPreserving(&fullPost, item.DirName, st.Posts[item.Post.ID]))
			if err := st.Save(); err != nil {
				c.Log.Printf("  warning: failed to save state: %v", err)
			}

		case actionFilesMissing:
			c.Log.Printf("  re-downloading: %s (%s)", item.Post.Title, item.Detail)
			dirName, err := savePost(c, blog, &item.Post)
			if err != nil {
				c.Log.Printf("  error: %v", err)
				continue
			}
			st.Add(item.Post.ID, postStateEntry(&item.Post, dirName))
			if err := st.Save(); err != nil {
				c.Log.Printf("  warning: failed to save state: %v", err)
			}

		case actionLocked:
			entry := st.Posts[item.Post.ID]
			entry.Locked = true
			st.Add(item.Post.ID, entry)
			if err := st.Save(); err != nil {
				c.Log.Printf("  warning: failed to save state: %v", err)
			}
		}
	}

	c.Log.Printf("Sync complete.")
	return nil
}

// checkMissingFiles verifies that expected files exist on disk for a post.
// Returns a detail string listing missing files, or empty if all present.
func checkMissingFiles(entry state.PostEntry, dir string) string {
	var missing []string

	if _, err := os.Stat(filepath.Join(dir, "post.json")); err != nil {
		missing = append(missing, "post.json")
	}
	if entry.HasComments {
		if _, err := os.Stat(filepath.Join(dir, "comments.json")); err != nil {
			missing = append(missing, "comments.json")
		}
	}
	if entry.HasMd {
		if _, err := os.Stat(filepath.Join(dir, "post.md")); err != nil {
			missing = append(missing, "post.md")
		}
	}

	return strings.Join(missing, ", ")
}

// checkVideoSizes validates local video files against remote for a post.
// Skips posts with no native video (ok_video) — nothing to verify. Otherwise
// fetches fresh video URLs and does authenticated HEAD requests, collecting
// all mismatches rather than bailing on the first one.
func checkVideoSizes(c *boosty.Client, blog string, post *boosty.Post, dir string) string {
	if !hasOkVideo(post.Data) {
		return ""
	}

	var fullPost boosty.Post
	if err := c.GetJSON(boosty.PostURL(blog, post.ID), &fullPost); err != nil {
		c.Log.Printf("  check-media %s: fetch failed: %v", post.ID, err)
		return ""
	}

	parsed := parser.ParseBlocks(fullPost.Data)
	var issues []string
	for _, m := range parsed.Media {
		if m.Type != "video" {
			continue
		}
		if issue := checkRemoteVideoSize(c.HTTP, boosty.UserAgent, c.Log,
			m.URL, filepath.Join(dir, m.Filename), m.Filename); issue != "" {
			issues = append(issues, issue)
		}
	}
	return strings.Join(issues, "; ")
}

// hasOkVideo reports whether any block is a native (ok_video) video.
func hasOkVideo(blocks []boosty.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "ok_video" {
			return true
		}
	}
	return false
}

// checkRemoteVideoSize compares local file size with the server's Content-Length
// obtained via HEAD. The okcdn signed URLs bind to the UA used to fetch them, so
// we must reuse the client's User-Agent.
//
// Returns a descriptive issue string on real mismatches (missing local, non-200,
// size differs). Transient problems (network error, missing Content-Length) are
// logged and return empty — they are not treated as mismatches to avoid flagging
// every post when the network is flaky.
func checkRemoteVideoSize(httpc *http.Client, ua string, log boosty.Logger,
	url, localPath, filename string) string {
	localInfo, err := os.Stat(localPath)
	if err != nil {
		return fmt.Sprintf("%s missing", filename)
	}

	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		log.Printf("  check-media %s: build request: %v", filename, err)
		return ""
	}
	req.Header.Set("User-Agent", ua)

	resp, err := httpc.Do(req)
	if err != nil {
		log.Printf("  check-media %s: HEAD error: %v", filename, err)
		return ""
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("%s: HEAD %d", filename, resp.StatusCode)
	}
	if resp.ContentLength <= 0 {
		log.Printf("  check-media %s: no Content-Length", filename)
		return ""
	}
	if localInfo.Size() != resp.ContentLength {
		return fmt.Sprintf("%s: local %s vs remote %s",
			filename,
			boosty.FormatSize(localInfo.Size()),
			boosty.FormatSize(resp.ContentLength),
		)
	}
	return ""
}
