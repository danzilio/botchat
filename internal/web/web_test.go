package web

import (
	"io"
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// spec: WEB-1
func TestServesEmbeddedUI(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	for path, want := range map[string]string{
		"/":          `<script src="app.js">`,
		"/app.js":    "EventSource",
		"/style.css": "--accent",
	} {
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Errorf("GET %s = %d, missing %q", path, res.StatusCode, want)
		}
		csp := res.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("GET %s: CSP %q must restrict resources to self", path, csp)
		}
	}
}

// spec: WEB-1
func TestNoExternalResources(t *testing.T) {
	// Any absolute URL in a src/href/url()/fetch/EventSource would load a
	// resource from another origin. The SVG XML namespace inside the inline
	// favicon is an identifier, not a request, so it is allowed.
	external := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//|url\(\s*["']?(https?:)?//|(fetch|EventSource|import)\s*\(\s*["'](https?:)?//|@import`)
	err := fs.WalkDir(FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(FS(), path)
		if loc := external.FindIndex(b); loc != nil {
			t.Errorf("%s references an external resource: %q", path, b[loc[0]:min(loc[1]+40, len(b))])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
