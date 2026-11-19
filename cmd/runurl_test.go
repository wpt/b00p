package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/syncer"
)

// apiToServer sends api.boosty.to requests to a test server.
type apiToServer struct{ target *url.URL }

func (a apiToServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if r.URL.Host == "api.boosty.to" {
		r.URL.Scheme, r.URL.Host = a.target.Scheme, a.target.Host
	}
	return http.DefaultTransport.RoundTrip(r)
}

func testClient(t *testing.T, h http.HandlerFunc) *boosty.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: apiToServer{u}}
	return &boosty.Client{Tokens: &boosty.Tokens{AccessToken: "t"}, HTTP: hc, DownloadHTTP: hc, Log: &stdLogger{}}
}

// --url refuses a payload it must not archive (no access, another post's
// id) before writing anything, and saves an accessible one.
func TestDownloadURL_StubGuard(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		wantErr bool
	}{
		{"no access", `{"id":"p1","title":"t","hasAccess":false}`, true},
		{"other post", `{"id":"p2","title":"t","hasAccess":true,"data":[{"type":"text","content":"[\"x\",\"unstyled\",[]]"}]}`, true},
		{"ok", `{"id":"p1","title":"t","hasAccess":true,"data":[{"type":"text","content":"[\"x\",\"unstyled\",[]]"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.payload))
			})
			out := t.TempDir()
			err := downloadURL(c, syncer.Config{Blog: "b", OutputDir: out}, "p1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("downloadURL = %v, want error %v", err, tc.wantErr)
			}
			matches, _ := filepath.Glob(filepath.Join(out, "b", "*", "post.json"))
			if tc.wantErr != (len(matches) == 0) {
				t.Errorf("post.json files = %v (error case must write none)", matches)
			}
			if !tc.wantErr {
				data, err := os.ReadFile(matches[0])
				var p struct{ ID string }
				if err != nil || json.Unmarshal(data, &p) != nil || p.ID != "p1" {
					t.Errorf("post.json = %s (%v)", data, err)
				}
			}
		})
	}
}

// Flags that do nothing in a mode are rejected, not ignored.
func TestRunDownload_RejectsFlagCombinations(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--url", "https://boosty.to/b/posts/p1", "--sync"}, "cannot be combined"},
		{[]string{"--url", "https://boosty.to/b/posts/p1", "--workers", "2"}, "--workers"},
		{[]string{"--blog", "b", "--check-media"}, "requires --sync"},
		{[]string{"--blog", "b", "--yes"}, "requires --sync"},
		{[]string{"--blog", "b", "--sync", "--force"}, "--force is only honored without --sync"},
		{[]string{"--blog", "b", "--url", "https://boosty.to/b/posts/p1"}, "mutually exclusive"},
		{[]string{"--blog", "b", "--workers", "0"}, "--workers must be"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			resetDownloadFlags(t)
			if err := downloadCmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			if err := runDownload(downloadCmd, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("runDownload = %v, want %q", err, tc.want)
			}
		})
	}
}

// buildConfig maps every download flag onto syncer.Config.
func TestBuildConfig_MapsFlags(t *testing.T) {
	resetDownloadFlags(t)
	if err := downloadCmd.ParseFlags([]string{"--md", "--comments", "--download-external", "--force",
		"--check-media", "--check-files", "--yes", "--workers", "3", "--format", "{id}", "-o", "out"}); err != nil {
		t.Fatal(err)
	}
	got := buildConfig("blog")
	want := syncer.Config{Blog: "blog", OutputDir: "out", DirFormat: "{id}", WithMD: true, WithComments: true,
		DownloadExternal: true, Force: true, CheckMedia: true, CheckFiles: true, AutoApply: true, Workers: 3}
	if got.Blog != want.Blog || got.OutputDir != want.OutputDir || got.DirFormat != want.DirFormat ||
		got.WithMD != want.WithMD || got.WithComments != want.WithComments || got.DownloadExternal != want.DownloadExternal ||
		got.Force != want.Force || got.CheckMedia != want.CheckMedia || got.CheckFiles != want.CheckFiles ||
		got.AutoApply != want.AutoApply || got.Workers != want.Workers {
		t.Errorf("buildConfig = %+v, want %+v", got, want)
	}
}

// resetDownloadFlags restores every download flag to its default after the
// test, so parsed flags do not leak between tests.
func resetDownloadFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		downloadCmd.Flags().VisitAll(func(f *pflag.Flag) {
			if err := f.Value.Set(f.DefValue); err != nil {
				t.Errorf("reset --%s: %v", f.Name, err)
			}
			f.Changed = false
		})
		rootCmd.PersistentFlags().VisitAll(func(f *pflag.Flag) {
			if err := f.Value.Set(f.DefValue); err != nil {
				t.Errorf("reset --%s: %v", f.Name, err)
			}
			f.Changed = false
		})
	}
	reset()
	t.Cleanup(reset)
}

// --since takes a date and makes no sense for a single post.
func TestRunDownload_SinceFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--blog", "b", "--since", "2024-13-01"}, "invalid --since"},
		{[]string{"--url", "https://boosty.to/b/posts/p1", "--since", "2024-01-01"}, "--since"},
	} {
		resetDownloadFlags(t)
		if err := downloadCmd.ParseFlags(tc.args); err != nil {
			t.Fatal(err)
		}
		if err := runDownload(downloadCmd, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: runDownload = %v, want %q", tc.args, err, tc.want)
		}
	}
}
