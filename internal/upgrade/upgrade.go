package upgrade

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// RepoOwner/RepoName point at the GitHub repository whose Releases publish
// the homer binaries. They are vars so tests can redirect lookups.
var (
	RepoOwner = "zzjcool"
	RepoName = "homer-cli"
)

// DefaultBaseURL is the release download root; overridable for tests and
// mirrors via HOMER_INSTALL_BASE_URL parity.
var DefaultBaseURL = "https://github.com/" + RepoOwner + "/" + RepoName + "/releases"

// Latest is the minimal release metadata the version check needs.
type Latest struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
}

// Client performs the network lookup with a hard timeout. The zero value is
// not usable; use NewClient.
type Client struct {
	HTTP    *http.Client
	BaseURL string
}

// NewClient builds a version-check client. An explicit baseURL (typically
// from HOMER_INSTALL_BASE_URL) redirects the check to a mirror; file://
// mirrors serve the latest tag from a LATEST marker file.
func NewClient(baseURL string) *Client {
	url := ""
	if strings.TrimSpace(baseURL) != "" {
		url = strings.TrimSpace(baseURL)
	}
	return &Client{
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		BaseURL: url,
	}
}

// FetchLatest returns the latest published release tag (for example
// "v1.3.1"). An error means the check could not be performed; callers treat
// that as "unknown" rather than "up to date".
func (c *Client) FetchLatest() (*Latest, error) {
	url := c.BaseURL
	if url == "" {
		url = "https://api.github.com/repos/" + RepoOwner + "/" + RepoName + "/releases/latest"
	}
	// Offline/mirror testing parity with install.sh: when the download root is
	// redirected via HOMER_INSTALL_BASE_URL, derive the "latest" tag from a
	// sibling LATEST file (goreleaser mirrors can publish one) instead of the
	// GitHub API. Absent the marker file, fall back to the real API.
	if strings.HasPrefix(url, "file://") {
		marker := strings.TrimSuffix(strings.TrimPrefix(url, "file://"), "/") + "/LATEST"
		if data, err := os.ReadFile(marker); err == nil {
			tag := strings.TrimSpace(string(data))
			if tag != "" {
				return &Latest{TagName: tag}, nil
			}
		}
		return nil, fmt.Errorf("file:// version source needs a LATEST marker file")
	}
	resp, err := c.HTTP.Get(url)
	if err != nil {
		return nil, fmt.Errorf("version check request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("version check returned HTTP %d", resp.StatusCode)
	}
	// Bound the response like the manifest port does; GitHub API JSON is small.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("version check read failed: %w", err)
	}
	var latest Latest
	if err := json.Unmarshal(body, &latest); err != nil {
		return nil, fmt.Errorf("version check parse failed: %w", err)
	}
	if strings.TrimSpace(latest.TagName) == "" {
		return nil, fmt.Errorf("version check response has no tag_name")
	}
	return &latest, nil
}

// IsNewer compares two version strings that may carry a leading "v". It
// compares dotted numeric segments; a longer segment list wins on equal
// prefixes (1.3 > 1.3.0 is false; 1.3.1 > 1.3 is true). Non-numeric segments
// fall back to string comparison for that segment.
func IsNewer(candidate, current string) bool {
	c := parseVersion(candidate)
	cur := parseVersion(current)
	if c == nil || cur == nil {
		return false
	}
	length := len(c)
	if len(cur) > length {
		length = len(cur)
	}
	for i := 0; i < length; i++ {
		var cs, cus segment
		if i < len(c) {
			cs = c[i]
		}
		if i < len(cur) {
			cus = cur[i]
		}
		if diff := cs.compare(cus); diff != 0 {
			return diff > 0
		}
	}
	return false
}

type segment struct {
	num     int
	numeric bool
	text    string
}

func (s segment) compare(other segment) int {
	if s.numeric && other.numeric {
		switch {
		case s.num < other.num:
			return -1
		case s.num > other.num:
			return 1
		}
		return 0
	}
	if s.numeric != other.numeric {
		// Numeric segments sort after non-numeric tails (1.3.1 > 1.3-beta).
		if s.numeric {
			return 1
		}
		return -1
	}
	return strings.Compare(s.text, other.text)
}

func parseVersion(raw string) []segment {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "v")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ".")
	segments := make([]segment, 0, len(parts))
	for _, part := range parts {
		numeric := true
		for _, r := range part {
			if r < '0' || r > '9' {
				numeric = false
				break
			}
		}
		if part == "" {
			numeric = false
		}
		seg := segment{text: part, numeric: numeric}
		if numeric {
			// Leading zeros are fine for comparison via atoi.
			n := 0
			for _, r := range part {
				n = n*10 + int(r-'0')
			}
			seg.num = n
		}
		segments = append(segments, seg)
	}
	return segments
}
