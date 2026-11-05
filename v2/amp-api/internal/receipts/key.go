package receipts

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxKeyBytes bounds a key's length.
const MaxKeyBytes = 512

// ValidateKey accepts an evidence key: segments of [A-Za-z0-9._-] joined by
// "/", with no "..", no "." segment, no leading or trailing "/", no "\\" and
// no empty segment. Keys are made by the server (Key), but every key that
// reaches a store, or comes back in a URL, is checked again here.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("evidence key is empty")
	}
	if len(key) > MaxKeyBytes {
		return fmt.Errorf("evidence key is longer than %d bytes", MaxKeyBytes)
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || strings.Contains(key, "..") {
		return fmt.Errorf("evidence key %q: no leading /, no \\ and no ..", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." {
			return fmt.Errorf("evidence key %q: empty segment", key)
		}
		for _, c := range seg {
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
			if !ok {
				return fmt.Errorf("evidence key %q: only [A-Za-z0-9._/-]", key)
			}
		}
	}
	return nil
}

// KeyProject is the project id a key starts with (0 when it doesn't).
func KeyProject(key string) int {
	first, _, _ := strings.Cut(key, "/")
	n, err := strconv.Atoi(first)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

var treeRe = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

// ValidTreeSHA reports whether s looks like a git object id (4 to 64 hex
// digits: abbreviated, SHA-1 or SHA-256).
func ValidTreeSHA(s string) bool { return treeRe.MatchString(s) }

// Key is the store key of a receipt's file:
// <project_id>/<task_id>/attempts/<receipt_id>/<tree_sha[:12]>/<name>.
func Key(projectID, taskID int, receiptID int64, treeSHA, name string) string {
	tree := strings.ToLower(treeSHA)
	if len(tree) > 12 {
		tree = tree[:12]
	}
	return fmt.Sprintf("%d/%d/attempts/%d/%s/%s", projectID, taskID, receiptID, tree, name)
}

var screenshotRe = regexp.MustCompile(`^screenshot-[0-9]{1,3}\.png$`)

// ContentType is the content type an allowlisted evidence name is stored
// and served as, and whether the name is allowed. The type always comes
// from the name, never from the uploader (no stored XSS through evidence).
func ContentType(name string) (string, bool) {
	switch name {
	case "video.mp4":
		return "video/mp4", true
	case "poster.gif":
		return "image/gif", true
	case "contact.png":
		return "image/png", true
	case "trace.zip":
		return "application/zip", true
	case "har.json", "receipt.json":
		return "application/json", true
	case "console.txt", "transcript.txt":
		return "text/plain; charset=utf-8", true
	}
	if screenshotRe.MatchString(name) {
		return "image/png", true
	}
	return "", false
}

// AllowedNames describes the allowlist, for error messages.
const AllowedNames = "video.mp4, poster.gif, contact.png, screenshot-<n>.png, trace.zip, har.json, receipt.json, console.txt, transcript.txt"

// Attachment reports whether a name is served as a download (attachment)
// rather than inline: archives and HAR files are never shown in a page.
func Attachment(name string) bool {
	return name == "trace.zip" || name == "har.json"
}
