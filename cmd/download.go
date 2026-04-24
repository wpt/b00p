package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
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
	autoApply        bool
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
	downloadCmd.Flags().BoolVar(&autoApply, "yes", false, "with --sync: skip the interactive confirmation and apply changes")
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

// postStateEntry builds a state.PostEntry from a post for the initial
// (NEW / JustUnlocked) save path. HasComments / HasMd reflect the current
// run's flags. The Edited / VideoMismatch / Missing apply path does NOT
// use this helper — it patches an existing entry in place so that failed
// writes do not advance UpdatedAt / CommentsCount past disk reality.
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

// dirReserver tracks which directory names are in flight or already owned by
// a given post ID, so two concurrent workers cannot pick the same target dir
// for posts whose formatted names collide. The previous resolveDirName only
// looked at the filesystem, which races between two workers that have not yet
// written post.json.
//
// A reservation is keyed by absolute blog dir + base name. Once owned by a
// post ID it is never released — even on failure — so a second post that
// would have collided is forced to a suffix instead of clobbering partial
// data on disk.
type dirReserver struct {
	mu    sync.Mutex
	owned map[string]string // key = blogDir + "\x00" + name → postID
}

func newDirReserver() *dirReserver {
	return &dirReserver{owned: make(map[string]string)}
}

// reserve returns a directory name (relative to blogDir) safe to use for the
// given postID. If base is unowned and either free on disk or already holds
// this post, base is returned. Otherwise the post ID is appended as a suffix
// so the caller never silently overwrites a sibling or a peer worker.
func (r *dirReserver) reserve(blogDir, postID, base string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if name, ok := r.tryName(blogDir, postID, base); ok {
		return name
	}
	suffix := postID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	candidate := base + "_" + suffix
	// Suffix collisions in practice require an 8-char hex prefix collision
	// AND a name collision; if it ever happens, fall through and accept it —
	// the on-disk post.json check still prevents data loss for the same ID.
	r.owned[blogDir+"\x00"+candidate] = postID
	return candidate
}

// tryName reports whether `name` can be used by postID. It returns the name
// when the in-process map either has no owner, or already names this post;
// or when the filesystem has no post.json or has one belonging to this post.
// The reservation is recorded on success.
func (r *dirReserver) tryName(blogDir, postID, name string) (string, bool) {
	key := blogDir + "\x00" + name
	if owner, ok := r.owned[key]; ok {
		if owner == postID {
			return name, true
		}
		return "", false
	}
	target := filepath.Join(blogDir, name)
	data, err := os.ReadFile(filepath.Join(target, "post.json"))
	if err != nil {
		r.owned[key] = postID
		return name, true
	}
	var existing struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &existing); err != nil || existing.ID == "" || existing.ID == postID {
		r.owned[key] = postID
		return name, true
	}
	return "", false
}

// reservations is the process-wide dir reserver. savePost calls into it.
var reservations = newDirReserver()

// savePost downloads a post's full content into the output directory.
// Returns the directory name actually used (which may include a collision
// suffix) so the caller can record it in state.
//
// A non-nil error means at least one required artifact (post.json, media,
// post.md when --md, comments.json when --comments) could not be written or
// downloaded. The caller MUST NOT record the post as downloaded in state on
// error — that is what makes the next sync re-attempt the failed pieces
// instead of silently leaving stale/missing files behind.
//
// External video failures are NOT fatal: --download-external is opt-in and
// depends on third-party sites that fail in routine ways (geo-blocks, age
// gates, dead links). They are logged and ignored for the state contract.
func savePost(c *boosty.Client, blog string, post *boosty.Post) (string, error) {
	if !post.HasAccess {
		c.Log.Printf("  skipping (no access): %s", post.Title)
		return "", nil
	}

	blogDir := filepath.Join(outputDir, blog)
	dirName := parser.FormatDirName(dirFormat, post.Title, post.PublishTime, post.ID)
	dirName = reservations.reserve(blogDir, post.ID, dirName)
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

	// Download media. Errors are joined and returned so the caller can refuse
	// to mark the post as downloaded in state.
	var errs []error
	if err := downloader.DownloadMedia(c, parsed.Media, dir); err != nil {
		errs = append(errs, fmt.Errorf("media: %w", err))
	}

	// External videos: opt-in and best-effort, only logged.
	if downloadExternal {
		if err := downloader.DownloadExternal(c.Log, parsed.Media, dir); err != nil {
			c.Log.Printf("  warning: external download error: %v", err)
		}
	}

	// Markdown
	if withMD {
		if err := writePostMarkdown(post, parsed, dir); err != nil {
			errs = append(errs, fmt.Errorf("post.md: %w", err))
		} else {
			c.Log.Printf("  saved post.md")
		}
	}

	// Comments
	if withComments {
		if err := downloadComments(c, blog, post.ID, dir); err != nil {
			errs = append(errs, fmt.Errorf("comments: %w", err))
		}
	}

	if len(errs) > 0 {
		return dirName, errors.Join(errs...)
	}
	return dirName, nil
}

// commentsPageLimit is the per-page limit for the comments listing endpoint.
// Boosty's offset query param is effectively ignored on the comments endpoint
// (offset>0 returns data=[] with isLast=true, so paginated fetching never
// advances past the first page), but the server honors limit values up to
// ~200 in a single call. 100 mirrors defaultReplyLimit and covers every post
// in observed blogs; posts with >100 top-level comments would silently cap
// here and surface on the next sync as a disk-vs-API mismatch via
// diskCommentCount, re-triggering a refetch (which still wouldn't help —
// real cursor pagination would be needed for >100 top-level threads).
const commentsPageLimit = 100

func downloadComments(c *boosty.Client, blog, postID, dir string) error {
	var allComments []boosty.Comment
	for comment, err := range c.FetchComments(blog, postID, commentsPageLimit) {
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

	st, err := state.Load(blogDir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
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

// syncItem is a per-post classification with independent flags. A single
// post can carry multiple flags simultaneously (e.g. edited AND comments
// changed AND video size mismatch) — the previous implementation used a
// single-action enum and silently dropped combined changes. The apply phase
// dispatches on each flag independently and re-fetches/re-downloads only
// what is actually needed.
type syncItem struct {
	Post     boosty.Post
	DirName  string
	Existing state.PostEntry // zero value if !InState
	InState  bool

	// Classification (set during phase 1; checkMedia/checkFilesFlag may add).
	IsNew             bool
	IsLockedNew       bool // brand new, no access
	JustLocked        bool // existed accessible, now locked
	JustUnlocked      bool // existed locked, now accessible
	Edited            bool // updatedAt changed
	NewComments       bool // disk-side count differs from post.Count.Comments
	BackfillUpdatedAt bool // existing.UpdatedAt was 0; needs persisting

	// DiskCommentCount is the count of top-level comments + their inlined replies
	// read from comments.json. Populated in classifyPost for posts in state with
	// HasComments=true. -1 means the file was missing, unreadable, or corrupt;
	// any non-negative value is directly comparable to post.Count.Comments. Used
	// instead of Existing.CommentsCount as the trigger source so legacy state
	// entries that cached an inflated API count cannot mask on-disk gaps.
	DiskCommentCount int

	VideoMismatch string       // detail string (empty = no mismatch)
	Missing       missingFiles // file existence check result
}

// IsActionable reports whether the item needs apply-phase work beyond a
// pure UpdatedAt backfill (which is persisted regardless).
func (s syncItem) IsActionable() bool {
	return s.IsNew || s.JustUnlocked || s.JustLocked ||
		s.Edited || s.NewComments ||
		s.VideoMismatch != "" || s.Missing.Any()
}

// Labels returns short status tags for display ordered by severity.
func (s syncItem) Labels() []string {
	var labels []string
	switch {
	case s.IsNew:
		labels = append(labels, "NEW")
	case s.IsLockedNew:
		labels = append(labels, "LOCKED_NEW")
	case s.JustLocked:
		labels = append(labels, "LOCKED")
	case s.JustUnlocked:
		labels = append(labels, "UNLOCKED")
	}
	if s.Edited {
		labels = append(labels, "UPDATED")
	}
	if s.NewComments {
		labels = append(labels, "COMMENTS")
	}
	if s.VideoMismatch != "" {
		labels = append(labels, "VIDEO_MISMATCH")
	}
	if s.Missing.Any() {
		labels = append(labels, "FILES_MISSING")
	}
	return labels
}

// Detail aggregates per-flag detail strings for display.
func (s syncItem) Detail() string {
	var parts []string
	if s.JustLocked {
		parts = append(parts, "was accessible, now locked")
	}
	if s.JustUnlocked {
		parts = append(parts, "was locked, now accessible")
	}
	if s.Edited {
		parts = append(parts, "post edited")
	}
	if s.NewComments {
		// Prefer the disk count when available — that's the value the trigger
		// fired on, and is what the user actually has locally. Fall back to the
		// state-cached count for posts that never had comments tracked.
		from := s.Existing.CommentsCount
		if s.Existing.HasComments && s.DiskCommentCount >= 0 {
			from = s.DiskCommentCount
		}
		parts = append(parts, fmt.Sprintf("comments: %d → %d",
			from, s.Post.Count.Comments))
	}
	if s.VideoMismatch != "" {
		parts = append(parts, s.VideoMismatch)
	}
	if m := s.Missing.String(); m != "" {
		parts = append(parts, "missing "+m)
	}
	return strings.Join(parts, "; ")
}

func syncBlog(c *boosty.Client, blog string) error {
	c.Log.Printf("Syncing %s...", blog)

	blogDir := filepath.Join(outputDir, blog)
	if err := os.MkdirAll(blogDir, 0755); err != nil {
		return err
	}

	st, err := state.Load(blogDir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// Phase 1: Fetch and classify.
	var items []syncItem
	for post, err := range c.FetchPosts(blog, 20) {
		if err != nil {
			return err
		}
		items = append(items, classifyPost(post, st, blogDir))
	}

	// Phase 2a: video size check (optional).
	if checkMedia {
		runCheckMedia(c, blog, blogDir, items)
	}

	// Phase 2b: files-on-disk check (optional).
	if checkFilesFlag {
		c.Log.Printf("Checking files on disk...")
		for i := range items {
			// Skip cases that already trigger a full re-download in apply.
			if !items[i].InState ||
				items[i].JustLocked || items[i].IsLockedNew ||
				items[i].JustUnlocked {
				continue
			}
			items[i].Missing = detectMissingFiles(items[i].Existing,
				filepath.Join(blogDir, items[i].DirName))
		}
	}

	// Phase 3: display + summary.
	displaySync(c, items)

	// Decide whether the apply phase has any work.
	hasActionable := false
	hasBackfill := false
	for _, item := range items {
		if item.IsActionable() {
			hasActionable = true
		}
		if item.BackfillUpdatedAt {
			hasBackfill = true
		}
	}

	if !hasActionable {
		// Persist any UpdatedAt backfills even when nothing else changed —
		// otherwise legacy entries stay at UpdatedAt=0 forever and edits
		// would be silently re-backfilled instead of detected.
		if hasBackfill {
			applyBackfill(st, items)
			if err := st.Save(); err != nil {
				c.Log.Printf("  warning: failed to save state: %v", err)
			}
		}
		c.Log.Printf("Everything up to date.")
		return nil
	}

	// Confirm. With --yes, skip the prompt entirely so headless runs
	// (cron, scripts, nohup pipelines) can apply without a TTY.
	if !confirmApply(c.Log, bufio.NewReader(os.Stdin), autoApply) {
		c.Log.Printf("Cancelled.")
		return nil
	}

	// Apply backfills before per-item changes; per-item updates use the
	// already-corrected st.Posts entries.
	applyBackfill(st, items)

	// Phase 4: apply.
	c.Log.Printf("Applying...")
	for _, item := range items {
		applyItem(c, blog, blogDir, st, item)
	}

	c.Log.Printf("Sync complete.")
	return nil
}

// confirmApply gates the apply phase of `--sync` behind a Y/N prompt.
// With auto=true (the --yes flag), the prompt is skipped entirely so
// headless callers (cron, nohup, scripts) can run without a TTY. The
// prompt itself is written to stdout (so the user sees it on a real
// terminal); only the structural log lines go through `log` so tests
// can observe behavior via a fake logger without capturing stdout.
func confirmApply(log boosty.Logger, in *bufio.Reader, auto bool) bool {
	log.Printf("")
	if auto {
		log.Printf("Auto-applying (--yes).")
		return true
	}
	fmt.Print("Apply changes? [y/N] ")
	answer, err := in.ReadString('\n')
	if err != nil {
		log.Printf("  warning: failed to read confirmation: %v", err)
		return false
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}

// classifyPost compares post against state and returns a syncItem with all
// applicable flags set.
func classifyPost(post boosty.Post, st *state.State, blogDir string) syncItem {
	dirName := parser.FormatDirName(dirFormat, post.Title, post.PublishTime, post.ID)
	existing, inState := st.Get(post.ID)

	item := syncItem{Post: post, DirName: dirName, DiskCommentCount: -1}

	if !inState {
		if post.HasAccess {
			item.IsNew = true
		} else {
			item.IsLockedNew = true
		}
		return item
	}

	item.Existing = existing
	item.InState = true
	item.DirName = existing.DirName

	if !post.HasAccess {
		if !existing.Locked {
			item.JustLocked = true
		}
		return item
	}

	if existing.Locked {
		// Was locked, now accessible — treat like UNLOCKED: full re-download.
		// Use the freshly-formatted dir name in case the title changed.
		item.JustUnlocked = true
		item.DirName = dirName
		return item
	}

	// State entries written before UpdatedAt was added to the schema have
	// UpdatedAt == 0; treating that as an edit would flag every such post
	// as UPDATED on first sync after upgrade. Require a known previous
	// value before declaring an edit.
	if existing.UpdatedAt != 0 && post.UpdatedAt != existing.UpdatedAt {
		item.Edited = true
	}
	if existing.UpdatedAt == 0 && post.UpdatedAt != 0 {
		item.BackfillUpdatedAt = true
	}

	// Comment-count trigger: prefer disk reality over the state-cached count.
	// The cached value is post.Count.Comments at last save, so for posts whose
	// Boosty count includes inlined replies that weren't actually saved (the
	// pre-reply_limit bug), state matches API while disk silently has fewer.
	// Reading comments.json catches that gap on the next sync without any flag.
	//
	// For posts the user never asked to track comments (HasComments=false) we
	// have no disk file to consult, so fall back to the legacy state-vs-API
	// comparison — preserves prior behavior for that case.
	if existing.HasComments {
		if n, ok := diskCommentCount(filepath.Join(blogDir, existing.DirName)); ok {
			item.DiskCommentCount = n
			if n != post.Count.Comments {
				item.NewComments = true
			}
		} else {
			// Missing or unreadable comments.json with HasComments=true is
			// itself a reason to refetch when the post has any comments.
			if post.Count.Comments > 0 {
				item.NewComments = true
			}
		}
	} else if post.Count.Comments != existing.CommentsCount {
		item.NewComments = true
	}

	return item
}

// diskCommentCount returns the on-disk equivalent of post.Count.Comments —
// top-level comments plus the replies that the API actually inlined into each
// of them. Returns ok=false when the file is missing, unreadable, or fails to
// parse; the caller treats that as a reason to refetch when the post has any
// comments at all. We only count len(c.Replies.Data) (not c.ReplyCount), since
// disk reflects what was stored, not what the server claims exists.
func diskCommentCount(dir string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "comments.json"))
	if err != nil {
		return 0, false
	}
	var comments []boosty.Comment
	if err := json.Unmarshal(data, &comments); err != nil {
		return 0, false
	}
	n := len(comments)
	for _, c := range comments {
		if c.Replies != nil {
			n += len(c.Replies.Data)
		}
	}
	return n, true
}

// runCheckMedia performs HEAD-based video size validation in parallel and
// records mismatches on the corresponding items. Items that already have
// IsNew/JustUnlocked/etc. set are skipped — they will get fresh media
// regardless via the apply phase.
func runCheckMedia(c *boosty.Client, blog, blogDir string, items []syncItem) {
	var jobs []int
	for i, item := range items {
		if !item.InState || !item.Post.HasAccess {
			continue
		}
		// Skip items that will be fully re-downloaded anyway.
		if item.JustLocked || item.JustUnlocked {
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
					// Distinct indices per worker → safe write.
					items[idx].VideoMismatch = mismatch
				}
			}
		})
	}
	wg.Wait()
}

// applyBackfill mutates st.Posts to record UpdatedAt for legacy entries.
// Idempotent and safe to call before or after applyItem; per-item updates
// still operate on the corrected entries.
func applyBackfill(st *state.State, items []syncItem) {
	for _, item := range items {
		if !item.BackfillUpdatedAt {
			continue
		}
		entry := st.Posts[item.Post.ID]
		entry.UpdatedAt = item.Post.UpdatedAt
		st.Posts[item.Post.ID] = entry
	}
}

// displaySync prints the actionable list and a per-flag summary.
func displaySync(c *boosty.Client, items []syncItem) {
	type counts struct {
		new, unlocked, locked, lockedNew                 int
		updated, comments, videoMismatch, filesMissing   int
		noChange                                         int
	}
	var k counts

	for _, item := range items {
		switch {
		case item.IsNew:
			k.new++
		case item.IsLockedNew:
			k.lockedNew++
		case item.JustLocked:
			k.locked++
		case item.JustUnlocked:
			k.unlocked++
		}
		if item.Edited {
			k.updated++
		}
		if item.NewComments {
			k.comments++
		}
		if item.VideoMismatch != "" {
			k.videoMismatch++
		}
		if item.Missing.Any() {
			k.filesMissing++
		}
		if !item.IsActionable() {
			k.noChange++
		}
	}

	for _, item := range items {
		if !item.IsActionable() {
			continue
		}
		labels := strings.Join(item.Labels(), ",")
		detail := ""
		if d := item.Detail(); d != "" {
			detail = " (" + d + ")"
		}
		c.Log.Printf("  [%s] %s%s", labels, item.Post.Title, detail)
	}

	c.Log.Printf("")
	c.Log.Printf("Sync summary:")
	if k.new > 0 {
		c.Log.Printf("  %d new posts", k.new)
	}
	if k.unlocked > 0 {
		c.Log.Printf("  %d unlocked posts", k.unlocked)
	}
	if k.updated > 0 {
		c.Log.Printf("  %d updated posts", k.updated)
	}
	if k.comments > 0 {
		c.Log.Printf("  %d comments updated", k.comments)
	}
	if k.videoMismatch > 0 {
		c.Log.Printf("  %d video size mismatches", k.videoMismatch)
	}
	if k.filesMissing > 0 {
		c.Log.Printf("  %d files missing on disk", k.filesMissing)
	}
	if k.locked > 0 {
		c.Log.Printf("  %d locked (data preserved)", k.locked)
	}
	if k.lockedNew > 0 {
		c.Log.Printf("  %d locked (no access)", k.lockedNew)
	}
	c.Log.Printf("  %d no changes", k.noChange)
}

// applyItem runs the apply phase for a single syncItem. Every flag is
// handled independently so combined changes (e.g. edited + comments +
// missing post.md) are all applied in a single pass instead of one of
// them being silently skipped.
func applyItem(c *boosty.Client, blog, blogDir string, st *state.State, item syncItem) {
	switch {
	case item.IsNew, item.JustUnlocked:
		c.Log.Printf("  downloading: %s", item.Post.Title)
		dirName, err := savePost(c, blog, &item.Post)
		if err != nil {
			c.Log.Printf("  error: %v", err)
			return
		}
		st.Add(item.Post.ID, postStateEntry(&item.Post, dirName))
		if err := st.Save(); err != nil {
			c.Log.Printf("  warning: failed to save state: %v", err)
		}
		return

	case item.JustLocked:
		entry := st.Posts[item.Post.ID]
		entry.Locked = true
		st.Add(item.Post.ID, entry)
		if err := st.Save(); err != nil {
			c.Log.Printf("  warning: failed to save state: %v", err)
		}
		return
	}

	if !item.IsActionable() {
		return // pure backfill or no-change; backfill already applied to st
	}

	// Update branch: for known-accessible posts we may need any combination
	// of post.json refresh, media re-download, post.md regeneration, and
	// comments fetch. We re-fetch the post once when any of those need
	// fresh data or signed media URLs.
	c.Log.Printf("  updating: %s — %s", item.Post.Title, item.Detail())
	dir := filepath.Join(blogDir, item.DirName)

	needPostJSON := item.Edited || item.Missing.PostJSON
	// Edited posts may have added/removed/replaced media — re-download.
	// VideoMismatch needs fresh signed URLs and a re-download.
	needMedia := item.Edited || item.VideoMismatch != ""
	// Markdown is regenerated when the post text or block list might have
	// changed (Edited) or when md is missing for an entry that previously
	// had it. Honor the current --md flag for missing-files re-download.
	needMD := (withMD || item.Existing.HasMd) && (item.Edited || item.Missing.Markdown)
	// Comments are fetched on count changes, on edits if previously tracked,
	// and when the comments file is gone but state says it should be there.
	needComments := item.NewComments ||
		(item.Edited && item.Existing.HasComments) ||
		item.Missing.Comments
	if withComments && (item.Edited || item.NewComments) {
		// User passed --comments on this run for an edited/new-comments
		// post that didn't previously have comments → fetch them now.
		needComments = true
	}

	// Need a fresh post when re-writing post.json, re-downloading media
	// (for fresh signed URLs), or regenerating markdown from current data.
	needFetch := needPostJSON || needMedia || needMD
	var fullPost boosty.Post
	if needFetch {
		if err := c.GetJSON(boosty.PostURL(blog, item.Post.ID), &fullPost); err != nil {
			c.Log.Printf("  error fetching post: %v", err)
			return
		}
	} else {
		fullPost = item.Post
	}

	// Per-artifact success tracking. Each "OK" flag governs whether the
	// corresponding state field is bumped below; a failed write must leave
	// the prior value in place so the next sync sees the same trigger and
	// retries instead of silently advancing past disk reality.
	//
	// "Not needed" counts as success — we only fail-close the fields whose
	// artefact this run was actually responsible for producing.
	postJSONOK := !needPostJSON
	if needPostJSON {
		if err := writeJSON(filepath.Join(dir, "post.json"), fullPost); err != nil {
			c.Log.Printf("  error writing post.json: %v", err)
		} else {
			postJSONOK = true
		}
	}

	parsed := parser.ParseBlocks(fullPost.Data)

	mediaOK := !needMedia
	if needMedia {
		if invalidateMediaForRedownload(parsed.Media, dir, item.Edited, c.Log) {
			if err := downloader.DownloadMedia(c, parsed.Media, dir); err != nil {
				c.Log.Printf("  error re-downloading media: %v", err)
			} else {
				mediaOK = true
			}
		}
	}

	mdOK := !needMD
	mdWrittenThisRun := false
	if needMD {
		if err := writePostMarkdown(&fullPost, parsed, dir); err != nil {
			c.Log.Printf("  error writing post.md: %v", err)
		} else {
			mdWrittenThisRun = true
			mdOK = true
		}
	}

	commentsOK := !needComments
	commentsWrittenThisRun := false
	if needComments {
		if err := downloadComments(c, blog, item.Post.ID, dir); err != nil {
			c.Log.Printf("  error: %v", err)
		} else {
			commentsWrittenThisRun = true
			commentsOK = true
		}
	}

	entry := buildSyncEntry(st.Posts[item.Post.ID], &fullPost, item.DirName,
		item.Edited, postJSONOK, mediaOK, mdOK, commentsOK,
		commentsWrittenThisRun, mdWrittenThisRun)
	st.Add(item.Post.ID, entry)
	if err := st.Save(); err != nil {
		c.Log.Printf("  warning: failed to save state: %v", err)
	}
}

// buildSyncEntry composes the post-apply state entry from the existing
// (post-backfill) entry plus per-artefact success flags. It is the
// single point where the contract "do not advance retry-controlling
// fields past what was verifiably persisted" is enforced:
//
//   - Title/DirName/Price/Tier are display metadata, refreshed unconditionally.
//   - UpdatedAt only advances when this run actually caught up with an
//     Edited post — meaning ALL four artefact channels that an edited
//     post can need (post.json, media, post.md, comments) either were
//     not required this run or completed successfully. Other triggers
//     (NewComments / VideoMismatch / Missing.*) do not change the remote
//     UpdatedAt, so we must not advance our cached copy off the back of
//     them.
//   - CommentsCount / HasComments only advance when comments were freshly
//     written this run.
//   - HasMd only advances to true when post.md was freshly written; an
//     existing true value is preserved by virtue of starting from `old`.
//
// The `*OK` parameters are the artefact gates (true when the artefact was
// not needed OR was written successfully). They drive UpdatedAt advance.
// `commentsWritten` and `mdWritten` are the stronger flags that flip
// HasComments / HasMd to true; they only differ from the gates when the
// artefact was not requested this run (in which case OK=true but
// Written=false, so the prior flag is preserved).
//
// Why all four? Edited posts can require any combination of post.json,
// media, post.md, and comments to be re-fetched. A failure in any one
// of them means the local mirror does not reflect the new UpdatedAt, so
// caching the new value would silently suppress the next sync's retry.
//
// Pure function — no I/O, suitable for table-driven testing of the bug
// where a failed artefact write used to silently bump UpdatedAt.
func buildSyncEntry(old state.PostEntry, fullPost *boosty.Post, dirName string,
	edited, postJSONOK, mediaOK, mdOK, commentsOK bool,
	commentsWritten, mdWritten bool,
) state.PostEntry {
	entry := old
	entry.Title = fullPost.Title
	entry.DirName = dirName
	entry.Price = fullPost.Price
	if fullPost.SubscriptionLevel != nil {
		entry.Tier = fullPost.SubscriptionLevel.Name
	} else {
		entry.Tier = ""
	}
	if edited && postJSONOK && mediaOK && mdOK && commentsOK {
		entry.UpdatedAt = fullPost.UpdatedAt
	}
	if commentsWritten {
		entry.CommentsCount = fullPost.Count.Comments
		entry.HasComments = true
	}
	if mdWritten {
		entry.HasMd = true
	}
	return entry
}

// invalidateMediaForRedownload removes local copies of media items so the
// subsequent DownloadMedia call actually fetches fresh bytes — DownloadFile
// skips existing non-empty files, so without removal an edited post with
// replaced media at the same filename (image_001.jpg etc.) would keep the
// stale local copy and we'd record success against new state.
//
// Removal scope:
//   - Always skip external_video (DownloadMedia also skips it).
//   - Pure VideoMismatch (edited=false) only invalidates videos.
//   - Edited invalidates every media item DownloadMedia would touch.
//
// Returns false if a removal failed for a reason other than ENOENT — in
// that case the caller MUST NOT proceed with download to avoid leaving
// the directory in a half-cleaned state. ENOENT is normal (the file
// might already be gone, or never existed) and treated as success.
func invalidateMediaForRedownload(media []parser.MediaItem, dir string,
	edited bool, log boosty.Logger,
) bool {
	for _, m := range media {
		if m.Type == "external_video" {
			continue
		}
		if !edited && m.Type != "video" {
			continue
		}
		p := filepath.Join(dir, m.Filename)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("  error: cannot remove %s: %v (skipping redownload)", p, err)
			return false
		}
	}
	return true
}

// missingFiles records which post artefacts are absent on disk. The flags
// drive the FILES_MISSING apply branch so we re-download only what is
// missing and preserve flags for files that still exist.
type missingFiles struct {
	PostJSON bool
	Comments bool
	Markdown bool
}

func (m missingFiles) Any() bool {
	return m.PostJSON || m.Comments || m.Markdown
}

// String returns a comma-separated list of missing filenames; empty if all
// present. Used both for sync display and as a stable test fixture.
func (m missingFiles) String() string {
	var parts []string
	if m.PostJSON {
		parts = append(parts, "post.json")
	}
	if m.Comments {
		parts = append(parts, "comments.json")
	}
	if m.Markdown {
		parts = append(parts, "post.md")
	}
	return strings.Join(parts, ", ")
}

// detectMissingFiles returns a struct describing which expected files are
// absent for a post. Comments and post.md are only checked when the prior
// state recorded them as previously downloaded.
func detectMissingFiles(entry state.PostEntry, dir string) missingFiles {
	var m missingFiles
	if _, err := os.Stat(filepath.Join(dir, "post.json")); err != nil {
		m.PostJSON = true
	}
	if entry.HasComments {
		if _, err := os.Stat(filepath.Join(dir, "comments.json")); err != nil {
			m.Comments = true
		}
	}
	if entry.HasMd {
		if _, err := os.Stat(filepath.Join(dir, "post.md")); err != nil {
			m.Markdown = true
		}
	}
	return m
}

// checkMissingFiles is the legacy string-returning helper, kept so existing
// tests that pin the formatted output still pass.
func checkMissingFiles(entry state.PostEntry, dir string) string {
	return detectMissingFiles(entry, dir).String()
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
